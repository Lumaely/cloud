package middleware

import (
	"cloud/src/services"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func newProtectedRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
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

	r := gin.New()
	r.GET("/private", AuthRequired(db, services.NewAuthService()), func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString("userID")+" "+c.GetString("sessionID"))
	})
	return r, mock
}

func doGet(r *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthRequiredRejectsBadHeaders(t *testing.T) {
	cases := map[string]string{
		"missing":      "",
		"wrong scheme": "Basic abc",
		"no token":     "Bearer",
		"extra parts":  "Bearer a b",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := newProtectedRouter(t)
			if w := doGet(r, header); w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestAuthRequiredUnknownSession(t *testing.T) {
	r, mock := newProtectedRouter(t)
	mock.ExpectQuery("SELECT user_id").
		WithArgs(services.NewAuthService().HashToken("tok")).
		WillReturnError(sql.ErrNoRows)

	if w := doGet(r, "Bearer tok"); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthRequiredDatabaseError(t *testing.T) {
	r, mock := newProtectedRouter(t)
	mock.ExpectQuery("SELECT user_id").WillReturnError(errors.New("boom"))

	if w := doGet(r, "Bearer tok"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestAuthRequiredValidSessionSetsUserID(t *testing.T) {
	r, mock := newProtectedRouter(t)
	mock.ExpectQuery("SELECT user_id").
		WithArgs(services.NewAuthService().HashToken("tok")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "session_id"}).AddRow("user-1", "session-1"))

	w := doGet(r, "Bearer tok")
	if w.Code != http.StatusOK || w.Body.String() != "user-1 session-1" {
		t.Errorf("got %d %q, want 200 \"user-1 session-1\"", w.Code, w.Body.String())
	}
}
