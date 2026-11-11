package test

import (
	"cloud/src/models"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (s *stack) upload(token, path, content string) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func (s *stack) used(token string) int64 {
	s.t.Helper()
	return decode[models.StorageUsage](s.t, s.do(http.MethodGet, "/users/me/storage", token, nil)).UsedBytes
}

func TestIntegrationAttachmentLifecycle(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("files@test.local")
	id := uuidV7(t)

	w := s.upload(a.token, "/attachments/"+id, "hello")
	expect(t, w, http.StatusCreated)
	if got := decode[models.Attachment](t, w); got.AttachmentID != id || got.SizeBytes != 5 || got.PreviewSizeBytes != 0 {
		t.Errorf("uploaded = %+v", got)
	}
	expect(t, s.upload(a.token, "/attachments/"+id, "again"), http.StatusConflict)

	w = s.do(http.MethodGet, "/attachments/"+id, a.token, nil)
	expect(t, w, http.StatusOK)
	if w.Body.String() != "hello" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download = %q %q", w.Body.String(), w.Header().Get("Content-Type"))
	}
	req := httptest.NewRequest(http.MethodGet, "/attachments/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Range", "bytes=1-2")
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	expect(t, w, http.StatusPartialContent)
	if w.Body.String() != "el" {
		t.Errorf("range = %q", w.Body.String())
	}

	expect(t, s.do(http.MethodGet, "/attachments/"+id+"/preview", a.token, nil), http.StatusNotFound)
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t)+"/preview", "p"), http.StatusNotFound)
	w = s.upload(a.token, "/attachments/"+id+"/preview", "pp")
	expect(t, w, http.StatusCreated)
	if got := decode[models.Attachment](t, w); got.PreviewSizeBytes != 2 {
		t.Errorf("with preview = %+v", got)
	}
	expect(t, s.upload(a.token, "/attachments/"+id+"/preview", "pp"), http.StatusConflict)
	if w := s.do(http.MethodGet, "/attachments/"+id+"/preview", a.token, nil); w.Body.String() != "pp" {
		t.Errorf("preview = %q", w.Body.String())
	}

	list := decode[[]models.Attachment](t, s.do(http.MethodGet, "/attachments", a.token, nil))
	if len(list) != 1 || list[0].AttachmentID != id || list[0].SizeBytes != 5 || list[0].PreviewSizeBytes != 2 {
		t.Errorf("list = %+v", list)
	}
	if s.used(a.token) != 5 {
		t.Errorf("used = %d, want 5", s.used(a.token))
	}

	expect(t, s.do(http.MethodDelete, "/attachments/"+id, a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodDelete, "/attachments/"+id, a.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodGet, "/attachments/"+id, a.token, nil), http.StatusNotFound)
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 0 {
		t.Errorf("objects after delete = %v", keys)
	}
	if list := decode[[]models.Attachment](t, s.do(http.MethodGet, "/attachments", a.token, nil)); len(list) != 0 {
		t.Errorf("list after delete = %+v", list)
	}
}

func TestIntegrationAttachmentValidation(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("badfiles@test.local")

	expect(t, s.upload(a.token, "/attachments/not-a-uuid", "x"), http.StatusBadRequest)
	expect(t, s.upload(a.token, "/attachments/"+strings.ToUpper(uuidV7(t)), "x"), http.StatusBadRequest)
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t), ""), http.StatusBadRequest)

	req := httptest.NewRequest(http.MethodPut, "/attachments/"+uuidV7(t), io.NopCloser(strings.NewReader("x")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+a.token)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	expect(t, w, http.StatusLengthRequired)

	id := uuidV7(t)
	expect(t, s.upload(a.token, "/attachments/"+id, "1"), http.StatusCreated)
	expect(t, s.upload(a.token, "/attachments/"+id+"/preview", strings.Repeat("p", 256<<10+1)), http.StatusRequestEntityTooLarge)
	expect(t, s.upload(a.token, "/attachments/"+id+"/preview", "22"), http.StatusCreated)
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 2 {
		t.Errorf("objects = %v, want file and preview", keys)
	}
}

func TestIntegrationAttachmentQuota(t *testing.T) {
	s := newStack(t, 10)
	a := s.register("quota@test.local")

	big := uuidV7(t)
	expect(t, s.upload(a.token, "/attachments/"+big, "12345678"), http.StatusCreated)
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t), "123"), http.StatusRequestEntityTooLarge)
	expect(t, s.upload(a.token, "/attachments/"+uuidV7(t), "12"), http.StatusCreated)

	expect(t, s.upload(a.token, "/attachments/"+big+"/preview", "123456789"), http.StatusBadRequest)
	expect(t, s.upload(a.token, "/attachments/"+big+"/preview", "12345678"), http.StatusCreated)
	if s.used(a.token) != 10 {
		t.Errorf("used = %d, want 10: previews are outside the quota", s.used(a.token))
	}
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 3 {
		t.Errorf("objects = %v, want 3: refused uploads leave none", keys)
	}
}

func TestIntegrationAttachmentsAreIsolated(t *testing.T) {
	s := newStack(t, 0)
	a, b := s.register("fa@test.local"), s.register("fb@test.local")
	id := uuidV7(t)
	expect(t, s.upload(a.token, "/attachments/"+id, "a's file"), http.StatusCreated)

	expect(t, s.do(http.MethodGet, "/attachments/"+id, b.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodDelete, "/attachments/"+id, b.token, nil), http.StatusNotFound)
	expect(t, s.upload(b.token, "/attachments/"+id+"/preview", "p"), http.StatusNotFound)
	expect(t, s.upload(b.token, "/attachments/"+id, "b's file"), http.StatusCreated)

	if w := s.do(http.MethodGet, "/attachments/"+id, a.token, nil); w.Body.String() != "a's file" {
		t.Errorf("a's file = %q", w.Body.String())
	}
}
