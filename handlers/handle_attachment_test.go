package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const attachmentID = "33333333-3333-3333-3333-333333333333"

func downloadRequest(id string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/attachments/"+id, nil)
}

func serveDownload(env *testEnv, id string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	router := newDownloadRouter(env)
	router.ServeHTTP(r, downloadRequest(id))
	return r
}

func newDownloadRouter(env *testEnv) *gin.Engine {
	r := gin.New()
	r.GET("/attachments/:attachment_id", func(c *gin.Context) {
		c.Set("userID", testUserID)
		c.Next()
	}, env.h.DownloadAttachment)
	return r
}

func expectAttachmentLookup(m sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
	return m.ExpectQuery("FROM entry_attachments").WithArgs(attachmentID, testUserID)
}

func TestDownloadAttachmentStreamsObject(t *testing.T) {
	env := newTestEnv(t)
	env.storage.objects = map[string]string{"u/e/obj": "\x89PNG-bytes"}
	expectAttachmentLookup(env.mock).
		WillReturnRows(sqlmock.NewRows([]string{"s3_object_key", "media_type"}).AddRow("u/e/obj", "image/png"))

	w := serveDownload(env, attachmentID)

	assertStatus(t, w, http.StatusOK)
	if w.Body.String() != "\x89PNG-bytes" {
		t.Errorf("body = %q", w.Body.String())
	}
	if w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Length") != "10" {
		t.Errorf("headers = %v", w.Header())
	}
}

func TestDownloadAttachmentErrors(t *testing.T) {
	found := func(m sqlmock.Sqlmock) {
		expectAttachmentLookup(m).
			WillReturnRows(sqlmock.NewRows([]string{"s3_object_key", "media_type"}).AddRow("k", "image/png"))
	}
	cases := []struct {
		name   string
		id     string
		setup  func(*testEnv)
		status int
	}{
		{"malformed id", "nope", nil, http.StatusBadRequest},
		{"not found or not owned", attachmentID, func(e *testEnv) {
			expectAttachmentLookup(e.mock).WillReturnError(sql.ErrNoRows)
		}, http.StatusNotFound},
		{"database error", attachmentID, func(e *testEnv) {
			expectAttachmentLookup(e.mock).WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"storage error", attachmentID, func(e *testEnv) {
			found(e.mock)
			e.storage.downloadErr = errBoom
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			assertStatus(t, serveDownload(env, tc.id), tc.status)
		})
	}
}
