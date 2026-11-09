package main

import (
	"bytes"
	"cloud/handlers"
	"cloud/services"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// stack is the real router wired to Postgres and MinIO.
type stack struct {
	t      *testing.T
	db     *sql.DB
	router *gin.Engine
	s3     *minio.Client
	bucket string
}

// Needs TEST_DATABASE_URL and MINIO_TEST_*; truncates that database.
func newStack(t *testing.T) *stack {
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

	schema, err := os.ReadFile("db/init/01_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE users, user_sessions, mood_entries, entry_attachments CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
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

	auth := services.NewAuthService()
	h := handlers.NewHandler(db, &services.MockStripeService{}, auth, storage)
	return &stack{t: t, db: db, router: newRouter(h, db, auth), s3: client, bucket: cfg.Bucket}
}

func (s *stack) do(req *http.Request, token string) *httptest.ResponseRecorder {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func (s *stack) postJSON(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return s.do(req, "")
}

func (s *stack) register(email string) (token, userID string) {
	s.t.Helper()
	w := s.postJSON("/users/register", `{"email":"`+email+`","password":"pw","full_name":"T"}`)
	expect(s.t, w, http.StatusCreated)
	var resp struct {
		Token string `json:"token"`
		User  struct {
			UserID string `json:"user_id"`
		} `json:"user"`
	}
	mustDecode(s.t, w, &resp)
	return resp.Token, resp.User.UserID
}

type attachment struct{ key, mediaType, content string }

func (s *stack) push(token string, fields map[string]string, files ...attachment) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for _, f := range files {
		mw.WriteField("encrypted_aes_key", f.key)
		mw.WriteField("media_type", f.mediaType)
		fw, _ := mw.CreateFormFile("file_content", "blob")
		io.WriteString(fw, f.content)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/sync", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return s.do(req, token)
}

func (s *stack) fetch(token, since string) []map[string]any {
	s.t.Helper()
	w := s.do(httptest.NewRequest(http.MethodGet, "/sync?since="+since, nil), token)
	expect(s.t, w, http.StatusOK)
	var entries []map[string]any
	mustDecode(s.t, w, &entries)
	return entries
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

func (s *stack) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func (s *stack) download(token, attachmentID string) *httptest.ResponseRecorder {
	return s.do(httptest.NewRequest(http.MethodGet, "/attachments/"+attachmentID, nil), token)
}

func (s *stack) attachments(token, since, entryID string) []map[string]any {
	s.t.Helper()
	for _, e := range s.fetch(token, since) {
		if e["entry_id"] == entryID {
			var out []map[string]any
			for _, a := range e["attachments"].([]any) {
				out = append(out, a.(map[string]any))
			}
			return out
		}
	}
	s.t.Fatalf("entry %s not in delta", entryID)
	return nil
}

func randomBytes(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func entry(hash string) map[string]string {
	return map[string]string{
		"mood_scale":      "5",
		"metadata_blob":   "meta",
		"sha256_hash":     hash,
		"entry_timestamp": "2026-11-09T10:00:00Z",
	}
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body %s", w.Code, status, w.Body.String())
	}
}

func mustDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

func TestIntegrationUserLifecycle(t *testing.T) {
	s := newStack(t)

	token, userID := s.register("alice@test.local")
	expect(t, s.postJSON("/users/register", `{"email":"alice@test.local","password":"pw"}`), http.StatusConflict)

	expect(t, s.postJSON("/users/login", `{"email":"alice@test.local","password":"bad"}`), http.StatusUnauthorized)
	expect(t, s.postJSON("/users/login", `{"email":"nobody@test.local","password":"pw"}`), http.StatusUnauthorized)
	login := s.postJSON("/users/login", `{"email":"alice@test.local","password":"pw"}`)
	expect(t, login, http.StatusOK)

	me := s.do(httptest.NewRequest(http.MethodGet, "/users/me", nil), token)
	expect(t, me, http.StatusOK)
	var user map[string]any
	mustDecode(t, me, &user)
	if user["user_id"] != userID || user["email"] != "alice@test.local" {
		t.Errorf("unexpected profile %v", user)
	}
	if n := s.count("SELECT count(*) FROM user_sessions WHERE user_id = $1", userID); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}

	expect(t, s.do(httptest.NewRequest(http.MethodDelete, "/users/me", nil), token), http.StatusNoContent)
	expect(t, s.do(httptest.NewRequest(http.MethodGet, "/users/me", nil), token), http.StatusUnauthorized)
	if n := s.count("SELECT count(*) FROM user_sessions WHERE user_id = $1", userID); n != 0 {
		t.Errorf("sessions not cascaded: %d left", n)
	}
}

func TestIntegrationOAuthUserCannotPasswordLogin(t *testing.T) {
	s := newStack(t)

	expect(t, s.postJSON("/users/register", `{"email":"oauth@test.local","oauth_id":"google-1"}`), http.StatusCreated)
	expect(t, s.postJSON("/users/login", `{"email":"oauth@test.local","password":"x"}`), http.StatusUnauthorized)
}

func TestIntegrationExpiredSessionRejected(t *testing.T) {
	s := newStack(t)
	token, _ := s.register("old@test.local")

	if _, err := s.db.Exec("UPDATE user_sessions SET expires_at = NOW() - INTERVAL '1 minute'"); err != nil {
		t.Fatal(err)
	}

	expect(t, s.do(httptest.NewRequest(http.MethodGet, "/users/me", nil), token), http.StatusUnauthorized)
}

func TestIntegrationSyncLifecycle(t *testing.T) {
	s := newStack(t)
	token, userID := s.register("sync@test.local")
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	created := s.push(token, entry("hash-1"), attachment{"aes-1", "image/png", "picture"})
	expect(t, created, http.StatusCreated)
	var resp map[string]string
	mustDecode(t, created, &resp)
	entryID := resp["entry_id"]

	keys := s.objectKeys(userID + "/" + entryID + "/")
	if len(keys) != 1 {
		t.Fatalf("objects = %v, want 1", keys)
	}
	obj, err := s.s3.GetObject(context.Background(), s.bucket, keys[0], minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(obj)
	info, _ := obj.Stat()
	if string(body) != "picture" || info.ContentType != "image/png" {
		t.Errorf("object = %q (%s)", body, info.ContentType)
	}
	var dbKey, mediaType string
	var aes []byte
	err = s.db.QueryRow("SELECT s3_object_key, encrypted_aes_key, media_type FROM entry_attachments WHERE entry_id = $1", entryID).
		Scan(&dbKey, &aes, &mediaType)
	if err != nil || dbKey != keys[0] || string(aes) != "aes-1" || mediaType != "image/png" {
		t.Errorf("attachment row = %q %q %q, err %v", dbKey, aes, mediaType, err)
	}

	expect(t, s.push(token, entry("hash-1")), http.StatusConflict)

	update := entry("hash-1")
	update["entry_id"] = entryID
	update["mood_scale"] = "7"
	expect(t, s.push(token, update), http.StatusCreated)

	entries := s.fetch(token, since)
	if len(entries) != 1 || entries[0]["entry_id"] != entryID || entries[0]["mood_scale"] != "Nw==" {
		t.Fatalf("delta after update = %v", entries)
	}
	if len(s.fetch(token, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))) != 0 {
		t.Error("future since returned entries")
	}

	expect(t, s.push(token, map[string]string{"entry_id": entryID, "is_deleted": "true"}), http.StatusOK)
	entries = s.fetch(token, since)
	if len(entries) != 1 || entries[0]["deleted_at"] == nil {
		t.Fatalf("tombstone missing: %v", entries)
	}

	expect(t, s.push(token, entry("hash-1")), http.StatusCreated)
}

func TestIntegrationSyncIsolatesUsers(t *testing.T) {
	s := newStack(t)
	alice, _ := s.register("a@test.local")
	bob, bobID := s.register("b@test.local")
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	w := s.push(alice, entry("alice-hash"))
	expect(t, w, http.StatusCreated)
	var resp map[string]string
	mustDecode(t, w, &resp)
	aliceEntry := resp["entry_id"]

	if got := s.fetch(bob, since); len(got) != 0 {
		t.Errorf("bob sees alice's entries: %v", got)
	}

	hijack := entry("bob-hash")
	hijack["entry_id"] = aliceEntry
	expect(t, s.push(bob, hijack, attachment{"k", "image/png", "x"}), http.StatusNotFound)
	if keys := s.objectKeys(bobID + "/"); len(keys) != 0 {
		t.Errorf("rejected push left objects: %v", keys)
	}
	if n := s.count("SELECT count(*) FROM entry_attachments WHERE entry_id = $1", aliceEntry); n != 0 {
		t.Errorf("bob attached %d files to alice's entry", n)
	}

	expect(t, s.push(bob, map[string]string{"entry_id": aliceEntry, "is_deleted": "true"}), http.StatusNotFound)
	if n := s.count("SELECT count(*) FROM mood_entries WHERE entry_id = $1 AND deleted_at IS NULL", aliceEntry); n != 1 {
		t.Error("bob deleted alice's entry")
	}
}

func TestIntegrationSyncRejectsMalformedEntryID(t *testing.T) {
	s := newStack(t)
	token, _ := s.register("m@test.local")

	update := entry("h")
	update["entry_id"] = "not-a-uuid"
	expect(t, s.push(token, update), http.StatusBadRequest)
	expect(t, s.push(token, map[string]string{"entry_id": "not-a-uuid", "is_deleted": "true"}), http.StatusBadRequest)
}

func TestIntegrationAttachmentRoundTrip(t *testing.T) {
	s := newStack(t)
	token, userID := s.register("files@test.local")
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	// 20 MiB exceeds minio-go's 16 MiB multipart threshold.
	small := randomBytes(t, 256*1024)
	large := randomBytes(t, 20*1024*1024)

	w := s.push(token, entry("files-1"),
		attachment{"aes-small", "image/png", small},
		attachment{"aes-large", "video/mp4", large})
	expect(t, w, http.StatusCreated)
	var resp map[string]string
	mustDecode(t, w, &resp)
	entryID := resp["entry_id"]

	atts := s.attachments(token, since, entryID)
	if len(atts) != 2 {
		t.Fatalf("attachments = %v, want 2", atts)
	}
	want := map[string]string{"image/png": small, "video/mp4": large}
	for _, a := range atts {
		got := s.download(token, a["attachment_id"].(string))
		expect(t, got, http.StatusOK)
		mediaType := a["media_type"].(string)
		if got.Header().Get("Content-Type") != mediaType {
			t.Errorf("Content-Type = %q, want %q", got.Header().Get("Content-Type"), mediaType)
		}
		if sha256.Sum256(got.Body.Bytes()) != sha256.Sum256([]byte(want[mediaType])) {
			t.Errorf("%s: downloaded %d bytes differ from uploaded %d", mediaType, got.Body.Len(), len(want[mediaType]))
		}
	}
	if len(s.objectKeys(userID+"/"+entryID+"/")) != 2 {
		t.Error("expected 2 objects in MinIO")
	}

	other, _ := s.register("other@test.local")
	expect(t, s.download(other, atts[0]["attachment_id"].(string)), http.StatusNotFound)
	expect(t, s.download(token, "33333333-3333-3333-3333-333333333333"), http.StatusNotFound)

	expect(t, s.push(token, map[string]string{"entry_id": entryID, "is_deleted": "true"}), http.StatusOK)
	if keys := s.objectKeys(userID + "/" + entryID + "/"); len(keys) != 0 {
		t.Errorf("tombstone left objects: %v", keys)
	}
	if n := s.count("SELECT count(*) FROM entry_attachments WHERE entry_id = $1", entryID); n != 0 {
		t.Errorf("tombstone left %d attachment rows", n)
	}
	expect(t, s.download(token, atts[0]["attachment_id"].(string)), http.StatusNotFound)
}

func TestIntegrationDeleteUserRemovesObjects(t *testing.T) {
	s := newStack(t)
	token, userID := s.register("gone@test.local")
	keeper, keeperID := s.register("keeper@test.local")
	expect(t, s.push(token, entry("g1"), attachment{"k", "image/png", "a"}), http.StatusCreated)
	expect(t, s.push(token, entry("g2"), attachment{"k", "image/png", "b"}), http.StatusCreated)
	expect(t, s.push(keeper, entry("k1"), attachment{"k", "image/png", "c"}), http.StatusCreated)

	expect(t, s.do(httptest.NewRequest(http.MethodDelete, "/users/me", nil), token), http.StatusNoContent)

	if keys := s.objectKeys(userID + "/"); len(keys) != 0 {
		t.Errorf("deleted user's objects remain: %v", keys)
	}
	if len(s.objectKeys(keeperID+"/")) != 1 {
		t.Error("other user's object was removed")
	}
}
