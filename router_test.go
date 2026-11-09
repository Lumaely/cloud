package main

import (
	"cloud/handlers"
	"cloud/services"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newTestRouter(t *testing.T, mode string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth := services.NewAuthService()
	h := handlers.NewHandler(db, &services.MockStripeService{}, auth, nil, nil, handlers.Settings{Mode: mode})
	r, err := newRouter(h, db, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func routes(r *gin.Engine) []string {
	var got []string
	for _, route := range r.Routes() {
		got = append(got, route.Method+" "+route.Path)
	}
	return got
}

var protectedRoutes = []string{
	"GET /users/me", "DELETE /users/me", "GET /users/me/keys", "PUT /users/me/password",
	"DELETE /users/me/sessions", "DELETE /users/me/diary", "GET /users/me/storage",
	"GET /sync", "POST /sync",
	"GET /attachments/:attachment_id", "GET /attachments/:attachment_id/preview", "DELETE /attachments/:attachment_id",
}

var resourcePaths = []string{"patterns", "activities", "places", "bookmark-folders", "bookmarks"}

func TestRouterRegistersRoutes(t *testing.T) {
	got := routes(newTestRouter(t, handlers.ModeCloud))

	want := append([]string{
		"GET /health", "POST /feedback",
		"POST /users/register", "POST /users/login/params", "POST /users/login",
		"POST /users/recover/start", "POST /users/recover/finish",
		"POST /users/erase/request", "POST /users/erase/confirm",
	}, protectedRoutes...)
	for _, res := range resourcePaths {
		want = append(want, "GET /"+res, "POST /"+res, "PUT /"+res+"/:id", "DELETE /"+res+"/:id")
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("routes =\n%v\nwant\n%v", got, want)
	}
}

func TestRouterSelfHostedHasNoEmailRoutes(t *testing.T) {
	for _, route := range routes(newTestRouter(t, handlers.ModeSelfHosted)) {
		if strings.HasPrefix(route, "POST /users/erase/") {
			t.Errorf("%s registered in self-hosted mode", route)
		}
	}
}

func TestRouterProtectsPrivateRoutes(t *testing.T) {
	r := newTestRouter(t, handlers.ModeCloud)
	all := slices.Clone(protectedRoutes)
	for _, res := range resourcePaths {
		all = append(all, "GET /"+res, "PUT /"+res+"/:id")
	}
	id := "33333333-3333-3333-3333-333333333333"
	for _, route := range all {
		method, path, _ := strings.Cut(route, " ")
		path = strings.NewReplacer(":attachment_id", id, ":id", id).Replace(path)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", route, w.Code)
		}
	}
}

func TestRouterRateLimitsAuthRoutes(t *testing.T) {
	r := newTestRouter(t, handlers.ModeCloud)
	var last int
	for range 11 {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/users/login", nil))
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("11th login = %d, want 429", last)
	}
}
