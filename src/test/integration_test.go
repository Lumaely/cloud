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
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type recordingMailer struct {
	mu     sync.Mutex
	emails []services.Email
}

func (m *recordingMailer) Send(_ context.Context, e services.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emails = append(m.emails, e)
	return nil
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
	h := handlers.NewHandler(db, &services.MockStripeService{}, auth, storage, mailer, handlers.Settings{
		Version: "test", Mode: handlers.ModeCloud, StorageQuota: quota, ServerSecret: []byte("test-secret"),
	})
	router, err := app.NewRouter(h, db, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &stack{t: t, db: db, router: router, s3: client, bucket: cfg.Bucket, mailer: mailer}
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
		"email":                  a.email,
		"auth_key":               a.authKey,
		"recovery_auth_key":      a.recoveryKey,
		"kdf_salt":               randomBytes(s.t, 16),
		"kdf_params":             map[string]any{"alg": "argon2id", "m": 65536, "t": 3, "p": 1},
		"wrapped_key_password":   []byte("wrapped-by-password"),
		"wrapped_key_recovery":   []byte("wrapped-by-recovery"),
		"encrypted_recovery_key": []byte("recovery-sealed-by-diary-key"),
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

type file struct {
	name, mediaType, content string
	preview                  string
}

func (s *stack) push(token string, ops []map[string]any, files ...file) []models.OperationResult {
	s.t.Helper()
	w := s.pushRaw(token, ops, files...)
	expect(s.t, w, http.StatusOK)
	return decode[struct {
		Results []models.OperationResult `json:"results"`
	}](s.t, w).Results
}

func (s *stack) pushRaw(token string, ops []map[string]any, files ...file) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	opsJSON, _ := json.Marshal(ops)
	mw.WriteField("operations", string(opsJSON))
	for _, f := range files {
		fw, _ := mw.CreateFormFile(f.name, f.name)
		io.WriteString(fw, f.content)
		if f.preview != "" {
			pw, _ := mw.CreateFormFile(f.name+"-preview", f.name)
			io.WriteString(pw, f.preview)
		}
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/sync", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func newEntry(ref, hash string, files ...file) map[string]any {
	op := map[string]any{
		"ref": ref, "entry_timestamp": "2026-11-09T10:00:00Z", "sha256_hash": hash,
		"mood_scale": []byte("5"), "metadata_blob": []byte("meta"),
	}
	var atts []map[string]any
	for _, f := range files {
		a := map[string]any{"file": f.name, "encrypted_aes_key": []byte("aes-" + f.name), "media_type": f.mediaType}
		if f.preview != "" {
			a["preview"] = f.name + "-preview"
		}
		atts = append(atts, a)
	}
	op["attachments"] = atts
	return op
}

func (s *stack) createEntry(token, hash string, files ...file) models.MoodEntry {
	s.t.Helper()
	res := s.push(token, []map[string]any{newEntry("x", hash, files...)}, files...)
	if res[0].Status != "created" {
		s.t.Fatalf("create entry: %+v", res[0])
	}
	return *res[0].Entry
}

func (s *stack) entries(token, query string) models.Page[models.MoodEntry] {
	s.t.Helper()
	w := s.do(http.MethodGet, "/sync"+query, token, nil)
	expect(s.t, w, http.StatusOK)
	return decode[models.Page[models.MoodEntry]](s.t, w)
}

func (s *stack) records(token, path string) models.Page[models.Record] {
	s.t.Helper()
	w := s.do(http.MethodGet, path, token, nil)
	expect(s.t, w, http.StatusOK)
	return decode[models.Page[models.Record]](s.t, w)
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

	keys := decode[models.KeyBundle](t, s.do(http.MethodGet, "/users/me/keys", a.token, nil))
	if string(keys.EncryptedRecoveryKey) != "recovery-sealed-by-diary-key" {
		t.Errorf("keys = %+v", keys)
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
			"recovery_auth_key": newRecovery, "wrapped_key_recovery": []byte("new-rec"), "encrypted_recovery_key": []byte("new-copy"),
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

func TestIntegrationEntrySyncLifecycle(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("sync@test.local")

	res := s.push(a.token, []map[string]any{
		newEntry("one", "h1", file{name: "f1", mediaType: "image/png", content: "pic", preview: "th"}),
		newEntry("two", "h2"),
		newEntry("bad", ""),
		{"ref": "ghost", "entry_id": "33333333-3333-3333-3333-333333333333", "version": 1,
			"entry_timestamp": "2026-11-09T10:00:00Z", "sha256_hash": "h9"},
	}, file{name: "f1", mediaType: "image/png", content: "pic", preview: "th"})
	statuses := fmt.Sprint(res[0].Status, res[1].Status, res[2].Status, res[3].Status)
	if statuses != "createdcreatedinvalidnot_found" {
		t.Fatalf("statuses = %s (%+v)", statuses, res)
	}
	one := *res[0].Entry
	if len(one.Attachments) != 1 || !one.Attachments[0].HasPreview || one.Attachments[0].SizeBytes != 3 || one.Attachments[0].PreviewSizeBytes != 2 || one.Version != 1 {
		t.Errorf("entry one = %+v", one)
	}

	first := s.entries(a.token, "?limit=1")
	if len(first.Items) != 1 || !first.HasMore || first.Total != 2 || first.DiaryEpoch != 1 {
		t.Fatalf("first page = %+v", first)
	}
	second := s.entries(a.token, "?limit=1&cursor="+strconv.FormatInt(first.NextCursor, 10))
	if len(second.Items) != 1 || second.HasMore || second.Total != 1 {
		t.Fatalf("second page = %+v", second)
	}
	cursor := second.NextCursor

	update := newEntry("u", "h1")
	update["entry_id"], update["version"], update["mood_scale"] = one.EntryID, 1, []byte("7")
	stale := newEntry("s", "h1-stale")
	stale["entry_id"], stale["version"] = one.EntryID, 1
	dup := newEntry("d", "h2")
	res = s.push(a.token, []map[string]any{update, stale, dup})
	if res[0].Status != "updated" || res[0].Entry.Version != 2 || string(res[0].Entry.MoodScale) != "7" {
		t.Errorf("update = %+v", res[0])
	}
	if res[1].Status != "conflict" || res[1].Entry.Version != 2 {
		t.Errorf("stale update = %+v", res[1])
	}
	if res[2].Status != "conflict" || res[2].Entry.SHA256Hash != "h2" {
		t.Errorf("duplicate = %+v", res[2])
	}

	delta := s.entries(a.token, "?cursor="+strconv.FormatInt(cursor, 10))
	if len(delta.Items) != 1 || delta.Items[0].EntryID != one.EntryID || len(delta.Items[0].Attachments) != 1 {
		t.Fatalf("delta after update = %+v", delta)
	}

	bookmark := s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b"), "entry_id": one.EntryID})
	expect(t, bookmark, http.StatusCreated)

	res = s.push(a.token, []map[string]any{{"ref": "del", "entry_id": one.EntryID, "version": 2, "deleted": true}})
	if res[0].Status != "deleted" {
		t.Fatalf("delete = %+v", res[0])
	}
	if keys := s.objectKeys(a.userID + "/" + one.EntryID + "/"); len(keys) != 0 {
		t.Errorf("deleted entry left objects %v", keys)
	}
	delta = s.entries(a.token, "?cursor="+strconv.FormatInt(delta.NextCursor, 10))
	if len(delta.Items) != 1 || delta.Items[0].DeletedAt == nil || len(delta.Items[0].MoodScale) != 0 {
		t.Errorf("tombstone = %+v", delta.Items)
	}
	bookmarks := s.records(a.token, "/bookmarks")
	if len(bookmarks.Items) != 1 || bookmarks.Items[0].DeletedAt == nil {
		t.Errorf("entry bookmark not tombstoned: %+v", bookmarks.Items)
	}

	res = s.push(a.token, []map[string]any{newEntry("again", "h1")})
	if res[0].Status != "created" {
		t.Errorf("re-push of deleted hash = %+v", res[0])
	}

	expect(t, s.pushRaw(a.token, nil), http.StatusBadRequest)
	expect(t, s.do(http.MethodGet, "/sync?limit=0", a.token, nil), http.StatusBadRequest)
}

func TestIntegrationEntriesAreIsolated(t *testing.T) {
	s := newStack(t, 0)
	alice, bob := s.register("a@test.local"), s.register("b@test.local")
	entry := s.createEntry(alice.token, "a1", file{name: "f", mediaType: "image/png", content: "x"})

	if got := s.entries(bob.token, ""); len(got.Items) != 0 {
		t.Errorf("bob sees %v", got.Items)
	}
	hijack := newEntry("h", "b1", file{name: "f", mediaType: "image/png", content: "y"})
	hijack["entry_id"], hijack["version"] = entry.EntryID, 1
	res := s.push(bob.token, []map[string]any{hijack, {"ref": "d", "entry_id": entry.EntryID, "deleted": true}},
		file{name: "f", mediaType: "image/png", content: "y"})
	if res[0].Status != "not_found" || res[1].Status != "not_found" {
		t.Errorf("bob's writes = %+v", res)
	}
	if keys := s.objectKeys(bob.userID + "/"); len(keys) != 0 {
		t.Errorf("rejected push left objects: %v", keys)
	}
	expect(t, s.do(http.MethodGet, "/attachments/"+entry.Attachments[0].AttachmentID, bob.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodDelete, "/attachments/"+entry.Attachments[0].AttachmentID, bob.token, nil), http.StatusNotFound)
}

func TestIntegrationAttachments(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("files@test.local")
	// 20 MiB exceeds minio-go's 16 MiB multipart threshold.
	large := string(randomBytes(t, 20<<20))
	entry := s.createEntry(a.token, "f1",
		file{name: "big", mediaType: "video/mp4", content: large},
		file{name: "pic", mediaType: "image/png", content: "0123456789", preview: "tiny"})
	big, pic := entry.Attachments[0], entry.Attachments[1]
	if big.HasPreview || !pic.HasPreview {
		t.Fatalf("attachments = %+v", entry.Attachments)
	}

	w := s.do(http.MethodGet, "/attachments/"+big.AttachmentID, a.token, nil)
	expect(t, w, http.StatusOK)
	if sha256.Sum256(w.Body.Bytes()) != sha256.Sum256([]byte(large)) || w.Header().Get("Content-Type") != "video/mp4" {
		t.Error("large download differs")
	}

	req := httptest.NewRequest(http.MethodGet, "/attachments/"+pic.AttachmentID, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Range", "bytes=2-5")
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	expect(t, w, http.StatusPartialContent)
	if w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Errorf("range = %q %q", w.Body.String(), w.Header().Get("Content-Range"))
	}

	w = s.do(http.MethodGet, "/attachments/"+pic.AttachmentID+"/preview", a.token, nil)
	expect(t, w, http.StatusOK)
	if w.Body.String() != "tiny" {
		t.Errorf("preview = %q", w.Body.String())
	}
	expect(t, s.do(http.MethodGet, "/attachments/"+big.AttachmentID+"/preview", a.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodGet, "/attachments/nope", a.token, nil), http.StatusBadRequest)

	cursor := s.entries(a.token, "").NextCursor
	expect(t, s.do(http.MethodDelete, "/attachments/"+pic.AttachmentID, a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodGet, "/attachments/"+pic.AttachmentID, a.token, nil), http.StatusNotFound)
	if keys := s.objectKeys(a.userID + "/" + entry.EntryID + "/"); len(keys) != 1 {
		t.Errorf("objects after delete = %v, want only the large file", keys)
	}
	delta := s.entries(a.token, "?cursor="+strconv.FormatInt(cursor, 10))
	if len(delta.Items) != 1 || delta.Items[0].Version != 2 || len(delta.Items[0].Attachments) != 1 {
		t.Errorf("entry after attachment delete = %+v", delta.Items)
	}
}

func TestIntegrationFileLimitAndQuota(t *testing.T) {
	s := newStack(t, 25)
	a := s.register("quota@test.local")

	var files []file
	for i := range 9 {
		files = append(files, file{name: fmt.Sprintf("f%d", i), mediaType: "image/png", content: "x"})
	}
	entry := s.createEntry(a.token, "q1", files...)

	usage := decode[models.StorageUsage](t, s.do(http.MethodGet, "/users/me/storage", a.token, nil))
	if usage.UsedBytes != 9 || usage.QuotaBytes != 25 {
		t.Errorf("usage = %+v", usage)
	}

	two := []file{{name: "n1", mediaType: "image/png", content: "y"}, {name: "n2", mediaType: "image/png", content: "z"}}
	op := newEntry("over", "q1", two...)
	op["entry_id"], op["version"] = entry.EntryID, 1
	if res := s.push(a.token, []map[string]any{op}, two...); res[0].Status != "invalid" {
		t.Errorf("11th file = %+v", res[0])
	}

	big := file{name: "big", mediaType: "image/png", content: "0123456789abcdefg"}
	if res := s.push(a.token, []map[string]any{newEntry("q", "q2", big)}, big); res[0].Status != "quota_exceeded" {
		t.Errorf("over quota = %+v", res[0])
	}
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 9 {
		t.Errorf("objects = %d, want 9", len(keys))
	}
}

func TestIntegrationResources(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("res@test.local")

	for _, path := range []string{"/patterns", "/activities", "/places", "/bookmark-folders"} {
		t.Run(path, func(t *testing.T) {
			w := s.do(http.MethodPost, path, a.token, map[string]any{"blob": []byte("v1")})
			expect(t, w, http.StatusCreated)
			rec := decode[models.Record](t, w)
			if rec.Version != 1 || string(rec.Blob) != "v1" {
				t.Fatalf("created = %+v", rec)
			}

			expect(t, s.do(http.MethodPost, path, a.token, map[string]any{}), http.StatusBadRequest)
			expect(t, s.do(http.MethodPut, path+"/"+rec.ID, a.token, map[string]any{"blob": []byte("x"), "version": 7}), http.StatusConflict)
			w = s.do(http.MethodPut, path+"/"+rec.ID, a.token, map[string]any{"blob": []byte("v2"), "version": 1})
			expect(t, w, http.StatusOK)
			if got := decode[models.Record](t, w); got.Version != 2 || string(got.Blob) != "v2" {
				t.Errorf("updated = %+v", got)
			}

			expect(t, s.do(http.MethodDelete, path+"/"+rec.ID, a.token, nil), http.StatusNoContent)
			expect(t, s.do(http.MethodDelete, path+"/"+rec.ID, a.token, nil), http.StatusNotFound)
			expect(t, s.do(http.MethodPut, path+"/"+rec.ID, a.token, map[string]any{"blob": []byte("x"), "version": 3}), http.StatusNotFound)

			page := s.records(a.token, path)
			if len(page.Items) != 1 || page.Items[0].DeletedAt == nil || len(page.Items[0].Blob) != 0 || page.Items[0].Version != 3 {
				t.Errorf("list = %+v", page.Items)
			}
		})
	}

	other := s.register("other@test.local")
	w := s.do(http.MethodPost, "/patterns", a.token, map[string]any{"blob": []byte("mine")})
	mine := decode[models.Record](t, w)
	if got := s.records(other.token, "/patterns"); len(got.Items) != 0 {
		t.Errorf("other user sees %v", got.Items)
	}
	expect(t, s.do(http.MethodPut, "/patterns/"+mine.ID, other.token, map[string]any{"blob": []byte("x"), "version": 1}), http.StatusNotFound)
	expect(t, s.do(http.MethodDelete, "/patterns/"+mine.ID, other.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodDelete, "/patterns/nope", a.token, nil), http.StatusBadRequest)
}

func TestIntegrationBookmarks(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("bm@test.local")
	other := s.register("bm2@test.local")
	entry := s.createEntry(a.token, "b1")
	otherEntry := s.createEntry(other.token, "b2")

	folder := decode[models.Record](t, s.do(http.MethodPost, "/bookmark-folders", a.token, map[string]any{"blob": []byte("Milestones")}))
	expect(t, s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b")}), http.StatusBadRequest)
	expect(t, s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b"), "entry_id": otherEntry.EntryID}), http.StatusBadRequest)
	expect(t, s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b"), "entry_id": entry.EntryID, "folder_id": "33333333-3333-3333-3333-333333333333"}), http.StatusBadRequest)

	w := s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b"), "entry_id": entry.EntryID, "folder_id": folder.ID})
	expect(t, w, http.StatusCreated)
	bm := decode[models.Record](t, w)
	if *bm.EntryID != entry.EntryID || *bm.FolderID != folder.ID {
		t.Fatalf("bookmark = %+v", bm)
	}

	cursor := s.records(a.token, "/bookmarks").NextCursor
	expect(t, s.do(http.MethodDelete, "/bookmark-folders/"+folder.ID, a.token, nil), http.StatusNoContent)
	moved := s.records(a.token, "/bookmarks?cursor="+strconv.FormatInt(cursor, 10))
	if len(moved.Items) != 1 || moved.Items[0].FolderID != nil || moved.Items[0].Version != 2 || moved.Items[0].DeletedAt != nil {
		t.Errorf("bookmark after folder delete = %+v", moved.Items)
	}
}

func TestIntegrationResetDiary(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("reset@test.local")
	s.createEntry(a.token, "r1", file{name: "f", mediaType: "image/png", content: "x"})
	expect(t, s.do(http.MethodPost, "/patterns", a.token, map[string]any{"blob": []byte("p")}), http.StatusCreated)

	expect(t, s.do(http.MethodDelete, "/users/me/diary", a.token, nil), http.StatusNoContent)

	page := s.entries(a.token, "")
	if len(page.Items) != 0 || page.DiaryEpoch != 2 {
		t.Errorf("entries after reset = %+v", page)
	}
	if got := s.records(a.token, "/patterns"); len(got.Items) != 0 {
		t.Errorf("patterns after reset = %+v", got.Items)
	}
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 0 {
		t.Errorf("objects after reset = %v", keys)
	}
	expect(t, s.login(a.email, a.authKey), http.StatusOK)
}

func TestIntegrationEraseAndStartOver(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("erase@test.local")
	s.createEntry(a.token, "e1", file{name: "f", mediaType: "image/png", content: "x"})

	expect(t, s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": "ghost@test.local"}), http.StatusAccepted)
	if len(s.mailer.emails) != 0 {
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
	if len(s.entries(resp.Token, "").Items) != 0 || len(s.objectKeys(a.userID+"/")) != 0 {
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

func TestIntegrationDeleteUserRemovesObjects(t *testing.T) {
	s := newStack(t, 0)
	gone, keeper := s.register("gone@test.local"), s.register("keeper@test.local")
	s.createEntry(gone.token, "g1", file{name: "f", mediaType: "image/png", content: "a"})
	s.createEntry(keeper.token, "k1", file{name: "f", mediaType: "image/png", content: "c"})

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
