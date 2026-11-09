package main

import (
	"cloud/handlers"
	"cloud/services"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newTestRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth := services.NewAuthService()
	h := handlers.NewHandler(db, &services.MockStripeService{}, auth, nil)
	return newRouter(h, db, auth), mock
}

func TestRouterRegistersRoutes(t *testing.T) {
	r, _ := newTestRouter(t)

	var got []string
	for _, route := range r.Routes() {
		got = append(got, route.Method+" "+route.Path)
	}
	sort.Strings(got)

	want := []string{
		"DELETE /users/me",
		"GET /attachments/:attachment_id",
		"GET /health",
		"GET /sync",
		"GET /users/me",
		"POST /sync",
		"POST /users/login",
		"POST /users/register",
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("routes = %v, want %v", got, want)
			break
		}
	}
}

func TestRouterPublicRoute(t *testing.T) {
	r, _ := newTestRouter(t)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", w.Code)
	}
}

func TestRouterProtectsPrivateRoutes(t *testing.T) {
	r, _ := newTestRouter(t)
	for _, route := range [][2]string{
		{http.MethodGet, "/users/me"},
		{http.MethodDelete, "/users/me"},
		{http.MethodGet, "/sync"},
		{http.MethodPost, "/sync"},
		{http.MethodGet, "/attachments/33333333-3333-3333-3333-333333333333"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(route[0], route[1], nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", route[0], route[1], w.Code)
		}
	}
}

func TestRouterPassesAuthenticatedRequests(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectQuery("FROM user_sessions").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("user-1"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sync", nil)
	req.Header.Set("Authorization", "Bearer tok")
	r.ServeHTTP(w, req)

	// 400 from FetchDelta proves auth passed.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
