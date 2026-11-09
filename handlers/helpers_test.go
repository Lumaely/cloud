package handlers

import (
	"bytes"
	"cloud/services"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

const testUserID = "11111111-1111-1111-1111-111111111111"

var errBoom = errors.New("boom")

type fakePayment struct {
	id  string
	err error
}

func (f *fakePayment) CreateCustomer(string, string) (string, error) { return f.id, f.err }

type upload struct {
	key, contentType, body string
}

type fakeStorage struct {
	mu              sync.Mutex
	uploads         []upload
	deleted         []string
	deletedPrefixes []string
	objects         map[string]string
	failOnNth       int // 1-based upload call that fails; 0 never fails
	deleteErr       error
	deletePrefixErr error
	downloadErr     error
}

func (f *fakeStorage) Upload(_ context.Context, key string, r io.Reader, _ int64, contentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnNth == len(f.uploads)+1 {
		return errBoom
	}
	body, _ := io.ReadAll(r)
	f.uploads = append(f.uploads, upload{key, contentType, string(body)})
	return nil
}

func (f *fakeStorage) Download(_ context.Context, key string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.downloadErr != nil {
		return nil, 0, f.downloadErr
	}
	body := f.objects[key]
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
}

func (f *fakeStorage) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedPrefixes = append(f.deletedPrefixes, prefix)
	return f.deletePrefixErr
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return f.deleteErr
}

type testEnv struct {
	h       *Handler
	mock    sqlmock.Sqlmock
	storage *fakeStorage
	payment *fakePayment
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	env := &testEnv{mock: mock, storage: &fakeStorage{}, payment: &fakePayment{id: "cus_test"}}
	env.h = NewHandler(db, env.payment, services.NewAuthService(), env.storage)
	return env
}

func serve(handler gin.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	r := gin.New()
	r.Handle(req.Method, req.URL.Path, func(c *gin.Context) {
		c.Set("userID", testUserID)
		c.Next()
	}, handler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func jsonRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type fileField struct {
	name, content string
}

func multipartRequest(t *testing.T, fields map[string][]string, files []fileField) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, values := range fields {
		for _, v := range values {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range files {
		fw, err := mw.CreateFormFile("file_content", f.name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, f.content)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/sync", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d, body %s", w.Code, want, w.Body.String())
	}
}
