// Package test holds integration tests that run against real Postgres and MinIO.
package test

import (
	"bytes"
	"cloud/src/app"
	"cloud/src/handlers"
	"cloud/src/models"
	"cloud/src/services"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type recordingMailer struct {
	mu     sync.Mutex
	fail   bool
	emails []services.Email
}

func (m *recordingMailer) Send(_ context.Context, e services.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("mailer down")
	}
	m.emails = append(m.emails, e)
	return nil
}

func (m *recordingMailer) setFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = fail
}

func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.emails)
}

func (m *recordingMailer) lastCode(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.emails) == 0 {
		t.Fatal("no email sent")
	}
	code := regexp.MustCompile(`\d{6}`).FindString(m.emails[len(m.emails)-1].Body)
	if code == "" {
		t.Fatal("no code in email")
	}
	return code
}

// stack is the real router wired to Postgres and MinIO.
type stack struct {
	t      *testing.T
	db     *sql.DB
	router *gin.Engine
	s3     *minio.Client
	bucket string
	mailer *recordingMailer
	h      *handlers.Handler
}

// Needs TEST_DATABASE_URL and MINIO_TEST_*; recreates the public schema of that database, then migrates it.
func newStack(t *testing.T, quota int64) *stack {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if dsn == "" || endpoint == "" {
		t.Skip("TEST_DATABASE_URL or MINIO_TEST_ENDPOINT not set")
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := app.Migrate(db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := services.S3Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("MINIO_TEST_ACCESS_KEY"),
		SecretKey: os.Getenv("MINIO_TEST_SECRET_KEY"),
		Bucket:    "it-stack-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Region:    "us-east-1",
	}
	storage, err := services.NewS3Storage(ctx, cfg)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Region: cfg.Region,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for obj := range client.ListObjects(ctx, cfg.Bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = client.RemoveObject(ctx, cfg.Bucket, obj.Key, minio.RemoveObjectOptions{})
		}
		_ = client.RemoveBucket(ctx, cfg.Bucket)
	})

	mailer := &recordingMailer{}
	auth := services.NewAuthService()
	h := handlers.NewHandler(db, auth, storage, mailer, handlers.Settings{
		Version: "test", Mode: handlers.ModeCloud, StorageQuota: quota, ServerSecret: []byte("test-secret"),
	})
	router, err := app.NewRouter(h, db, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &stack{t: t, db: db, router: router, s3: client, bucket: cfg.Bucket, mailer: mailer, h: h}
}

func (s *stack) do(method, path, token string, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// account holds what a client derives locally; the server only sees the keys and sealed blobs.
type account struct {
	email       string
	authKey     []byte
	recoveryKey []byte
	token       string
	userID      string
}

func (s *stack) keyFields(a *account) map[string]any {
	return map[string]any{
		"email":                a.email,
		"auth_key":             a.authKey,
		"recovery_auth_key":    a.recoveryKey,
		"kdf_salt":             randomBytes(s.t, 16),
		"kdf_params":           map[string]any{"alg": "argon2id", "m": 65536, "t": 3, "p": 1},
		"wrapped_key_password": []byte("wrapped-by-password"),
		"wrapped_key_recovery": []byte("wrapped-by-recovery"),
	}
}

func (s *stack) register(email string) *account {
	s.t.Helper()
	a := &account{email: email, authKey: randomBytes(s.t, 32), recoveryKey: randomBytes(s.t, 32)}
	w := s.do(http.MethodPost, "/users/register", "", s.keyFields(a))
	expect(s.t, w, http.StatusCreated)
	resp := decode[models.AuthResponse](s.t, w)
	a.token, a.userID = resp.Token, resp.User.UserID
	return a
}

func (s *stack) login(email string, authKey []byte) *httptest.ResponseRecorder {
	return s.do(http.MethodPost, "/users/login", "", map[string]any{"email": email, "auth_key": authKey})
}

func (s *stack) objectKeys(prefix string) []string {
	var keys []string
	for obj := range s.s3.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			s.t.Fatal(obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	return keys
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body %s", w.Code, status, w.Body.String())
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func TestIntegrationRegisterAndLogin(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("Alice@Test.local")

	expect(t, s.do(http.MethodPost, "/users/register", "", s.keyFields(&account{email: "alice@test.local", authKey: a.authKey, recoveryKey: a.recoveryKey})), http.StatusConflict)

	params := decode[models.LoginParamsResponse](t, s.do(http.MethodPost, "/users/login/params", "", map[string]string{"email": "ALICE@test.local"}))
	if len(params.KDFSalt) != 16 || !bytes.Contains(params.KDFParams, []byte("argon2id")) {
		t.Errorf("login params = %+v", params)
	}
	fake1 := decode[models.LoginParamsResponse](t, s.do(http.MethodPost, "/users/login/params", "", map[string]string{"email": "ghost@test.local"}))
	fake2 := decode[models.LoginParamsResponse](t, s.do(http.MethodPost, "/users/login/params", "", map[string]string{"email": "ghost@test.local"}))
	if len(fake1.KDFSalt) != 16 || !bytes.Equal(fake1.KDFSalt, fake2.KDFSalt) {
		t.Error("unknown email salt is not stable")
	}

	expect(t, s.login("alice@test.local", randomBytes(t, 32)), http.StatusUnauthorized)
	expect(t, s.login("ghost@test.local", a.authKey), http.StatusUnauthorized)
	w := s.login("alice@test.local", a.authKey)
	expect(t, w, http.StatusOK)
	resp := decode[models.AuthResponse](t, w)
	if string(resp.Keys.WrappedKeyPassword) != "wrapped-by-password" || resp.User.DiaryEpoch != 1 || *resp.User.Email != "alice@test.local" {
		t.Errorf("login response = %+v", resp)
	}

	keys := s.do(http.MethodGet, "/users/me/keys", a.token, nil)
	if got := decode[models.KeyBundle](t, keys); string(got.WrappedKeyRecovery) != "wrapped-by-recovery" || strings.Contains(keys.Body.String(), "encrypted_recovery_key") {
		t.Errorf("keys = %s", keys.Body.String())
	}
	var stripeColumns int
	if err := s.db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name LIKE 'stripe%'`).Scan(&stripeColumns); err != nil || stripeColumns != 0 {
		t.Errorf("users keeps %d stripe columns, err %v", stripeColumns, err)
	}
	me := decode[models.User](t, s.do(http.MethodGet, "/users/me", a.token, nil))
	if me.UserID != a.userID {
		t.Errorf("me = %+v", me)
	}

	bad := s.keyFields(&account{email: "b@test.local", authKey: []byte("short"), recoveryKey: a.recoveryKey})
	expect(t, s.do(http.MethodPost, "/users/register", "", bad), http.StatusBadRequest)
}

func TestIntegrationRecovery(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("rec@test.local")
	start := func(key []byte) *httptest.ResponseRecorder {
		return s.do(http.MethodPost, "/users/recover/start", "", map[string]any{"email": a.email, "recovery_auth_key": key})
	}

	expect(t, start(randomBytes(t, 32)), http.StatusUnauthorized)
	w := start(a.recoveryKey)
	expect(t, w, http.StatusOK)
	started := decode[models.RecoverStartResponse](t, w)
	if string(started.WrappedKeyRecovery) != "wrapped-by-recovery" || len(started.RecoveryToken) != 64 {
		t.Fatalf("start = %+v", started)
	}

	newKey := randomBytes(t, 32)
	finish := map[string]any{
		"recovery_token": started.RecoveryToken, "auth_key": newKey,
		"kdf_salt": randomBytes(t, 16), "kdf_params": map[string]any{"alg": "argon2id"},
		"wrapped_key_password": []byte("rewrapped"),
	}
	expect(t, s.do(http.MethodPost, "/users/recover/finish", "", map[string]any{"recovery_token": "nope", "auth_key": newKey}), http.StatusBadRequest)
	w = s.do(http.MethodPost, "/users/recover/finish", "", finish)
	expect(t, w, http.StatusOK)
	if string(decode[models.AuthResponse](t, w).Keys.WrappedKeyPassword) != "rewrapped" {
		t.Error("password key not replaced")
	}

	expect(t, s.do(http.MethodPost, "/users/recover/finish", "", finish), http.StatusUnauthorized)
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusUnauthorized)
	expect(t, s.login(a.email, a.authKey), http.StatusUnauthorized)
	expect(t, s.login(a.email, newKey), http.StatusOK)
}

func TestIntegrationChangePassword(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("pw@test.local")
	other := decode[models.AuthResponse](t, s.login(a.email, a.authKey)).Token
	newKey, newRecovery := randomBytes(t, 32), randomBytes(t, 32)
	body := map[string]any{
		"current_auth_key": randomBytes(t, 32), "auth_key": newKey,
		"kdf_salt": randomBytes(t, 16), "kdf_params": map[string]any{"alg": "argon2id"},
		"wrapped_key_password": []byte("new-wrap"),
		"recovery": map[string]any{
			"recovery_auth_key": newRecovery, "wrapped_key_recovery": []byte("new-rec"),
		},
		"sign_out_other_devices": true,
	}

	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, body), http.StatusUnauthorized)
	body["current_auth_key"] = a.authKey
	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, body), http.StatusNoContent)

	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusOK)
	expect(t, s.do(http.MethodGet, "/users/me", other, nil), http.StatusUnauthorized)
	expect(t, s.login(a.email, a.authKey), http.StatusUnauthorized)
	expect(t, s.login(a.email, newKey), http.StatusOK)
	expect(t, s.do(http.MethodPost, "/users/recover/start", "", map[string]any{"email": a.email, "recovery_auth_key": a.recoveryKey}), http.StatusUnauthorized)
	expect(t, s.do(http.MethodPost, "/users/recover/start", "", map[string]any{"email": a.email, "recovery_auth_key": newRecovery}), http.StatusOK)
}

func TestIntegrationSignOutAllDevices(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("out@test.local")
	other := decode[models.AuthResponse](t, s.login(a.email, a.authKey)).Token

	expect(t, s.do(http.MethodDelete, "/users/me/sessions", a.token, nil), http.StatusNoContent)

	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusUnauthorized)
	expect(t, s.do(http.MethodGet, "/users/me", other, nil), http.StatusUnauthorized)
}

func TestIntegrationExpiredSessionRejected(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("old@test.local")
	if _, err := s.db.Exec("UPDATE user_sessions SET expires_at = NOW() - INTERVAL '1 minute'"); err != nil {
		t.Fatal(err)
	}
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusUnauthorized)
}

func TestIntegrationResetDiary(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("reset@test.local")
	s.create(a.token, "entries", dayID(t), "day")
	s.create(a.token, "patterns", uuidV7(t), "p")
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t), "x"), http.StatusCreated)

	expect(t, s.do(http.MethodDelete, "/users/me/diary", a.token, nil), http.StatusNoContent)

	page := s.changes(a.token, "")
	if len(page.Items) != 0 || page.DiaryEpoch != 2 {
		t.Errorf("changes after reset = %+v", page)
	}
	if list := decode[[]models.Attachment](t, s.do(http.MethodGet, "/attachments", a.token, nil)); len(list) != 0 || s.used(a.token) != 0 {
		t.Errorf("attachments after reset = %+v", list)
	}
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 0 {
		t.Errorf("objects after reset = %v", keys)
	}
	expect(t, s.login(a.email, a.authKey), http.StatusOK)
}

func TestIntegrationEraseAndStartOver(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("erase@test.local")
	s.create(a.token, "entries", dayID(t), "day")
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t), "x"), http.StatusCreated)

	expect(t, s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": "ghost@test.local"}), http.StatusAccepted)
	if s.mailer.count() != 0 {
		t.Fatal("email sent for unknown account")
	}
	expect(t, s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": a.email}), http.StatusAccepted)
	code := s.mailer.lastCode(t)

	newKey := randomBytes(t, 32)
	confirm := s.keyFields(&account{email: a.email, authKey: newKey, recoveryKey: randomBytes(t, 32)})
	confirm["code"] = "000000"
	if code == "000000" {
		confirm["code"] = "111111"
	}
	expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusUnauthorized)

	confirm["code"] = code
	w := s.do(http.MethodPost, "/users/erase/confirm", "", confirm)
	expect(t, w, http.StatusOK)
	resp := decode[models.AuthResponse](t, w)
	if resp.User.DiaryEpoch != 2 {
		t.Errorf("epoch = %d, want 2", resp.User.DiaryEpoch)
	}
	if len(s.changes(resp.Token, "").Items) != 0 || s.used(resp.Token) != 0 || len(s.objectKeys(a.userID+"/")) != 0 {
		t.Error("diary not erased")
	}
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusUnauthorized)
	expect(t, s.login(a.email, a.authKey), http.StatusUnauthorized)
	expect(t, s.login(a.email, newKey), http.StatusOK)
	expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusUnauthorized)
}

func TestIntegrationEraseCodeBurnsAfterFiveFailures(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("burn@test.local")
	expect(t, s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": a.email}), http.StatusAccepted)
	code := s.mailer.lastCode(t)

	confirm := s.keyFields(&account{email: a.email, authKey: randomBytes(t, 32), recoveryKey: randomBytes(t, 32)})
	confirm["code"] = "wrong"
	for range 5 {
		expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusUnauthorized)
	}
	confirm["code"] = code
	expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusUnauthorized)
}

func TestIntegrationEraseRequestThrottle(t *testing.T) {
	s := newStack(t, 0)
	s.register("throttle@test.local")
	request := func(email string) *httptest.ResponseRecorder {
		return s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": email})
	}

	expect(t, request("THROTTLE@test.local"), http.StatusAccepted)
	expect(t, request("Throttle@Test.local"), http.StatusAccepted)
	expect(t, request("throttle@test.local"), http.StatusAccepted)
	if n := s.mailer.count(); n != 1 {
		t.Fatalf("emails within 60 s = %d, want 1", n)
	}

	if _, err := s.db.Exec(`UPDATE email_codes SET sent_at = NOW() - INTERVAL '61 seconds'`); err != nil {
		t.Fatal(err)
	}
	expect(t, request("throttle@test.local"), http.StatusAccepted)
	if n := s.mailer.count(); n != 2 {
		t.Errorf("emails after 60 s = %d, want 2", n)
	}
}

func TestIntegrationEraseRequestThrottleConcurrent(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("race@test.local")

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": a.email})
		})
	}
	wg.Wait()
	if n := s.mailer.count(); n != 1 {
		t.Errorf("concurrent requests sent %d emails, want 1", n)
	}
}

func TestIntegrationEraseRequestSendFailure(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("fail@test.local")
	request := map[string]string{"email": a.email}

	s.mailer.setFail(true)
	w := s.do(http.MethodPost, "/users/erase/request", "", request)
	expect(t, w, http.StatusServiceUnavailable)
	if got := decode[map[string]string](t, w)["error"]; got != "Failed to send email" {
		t.Errorf("error = %q", got)
	}

	s.mailer.setFail(false)
	expect(t, s.do(http.MethodPost, "/users/erase/request", "", request), http.StatusAccepted)
	confirm := s.keyFields(&account{email: a.email, authKey: randomBytes(t, 32), recoveryKey: randomBytes(t, 32)})
	confirm["code"] = s.mailer.lastCode(t)
	expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusOK)
}

func TestIntegrationDeleteUserRemovesObjects(t *testing.T) {
	s := newStack(t, 0)
	gone, keeper := s.register("gone@test.local"), s.register("keeper@test.local")
	expect(t, s.upload(gone.token, "/attachments/"+uuidV7(t), "a"), http.StatusCreated)
	expect(t, s.upload(keeper.token, "/attachments/"+uuidV7(t), "c"), http.StatusCreated)

	expect(t, s.do(http.MethodDelete, "/users/me", gone.token, nil), http.StatusNoContent)

	if keys := s.objectKeys(gone.userID + "/"); len(keys) != 0 {
		t.Errorf("deleted user's objects remain: %v", keys)
	}
	if len(s.objectKeys(keeper.userID+"/")) != 1 {
		t.Error("other user's object was removed")
	}
	expect(t, s.login(gone.email, gone.authKey), http.StatusUnauthorized)
}

func TestIntegrationFeedback(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("fb@test.local")

	expect(t, s.do(http.MethodPost, "/feedback", "", map[string]any{"message": "hi", "platform": "ios"}), http.StatusCreated)
	expect(t, s.do(http.MethodPost, "/feedback", a.token, map[string]any{"message": "from me", "contact_email": "Me@Test.local"}), http.StatusCreated)
	expect(t, s.do(http.MethodPost, "/feedback", "", map[string]any{}), http.StatusBadRequest)
	expect(t, s.do(http.MethodPost, "/feedback", "", map[string]any{"message": "x", "contact_email": "nope"}), http.StatusBadRequest)

	var linked int
	var contact string
	err := s.db.QueryRow(`SELECT count(*) FILTER (WHERE user_id = $1), max(contact_email) FROM feedback`, a.userID).Scan(&linked, &contact)
	if err != nil || linked != 1 || contact != "me@test.local" {
		t.Errorf("feedback rows: linked=%d contact=%q err=%v", linked, contact, err)
	}
}

func TestIntegrationHealth(t *testing.T) {
	s := newStack(t, 0)
	got := decode[map[string]string](t, s.do(http.MethodGet, "/health", "", nil))
	if got["version"] != "test" || got["mode"] != handlers.ModeCloud {
		t.Errorf("health = %v", got)
	}
}
