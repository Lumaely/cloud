package app

import (
	"cloud/src/handlers"
	"cloud/src/services"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
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
	h := handlers.NewHandler(db, auth, nil, nil, handlers.Settings{Mode: mode})
	r, err := NewRouter(h, db, auth, nil)
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
	"GET /users/me/sessions", "DELETE /users/me/sessions", "DELETE /users/me/sessions/:session_id", "DELETE /users/me/diary", "GET /users/me/storage",
	"GET /changes", "POST /changes",
	"GET /attachments", "PUT /attachments/:attachment_id", "GET /attachments/:attachment_id", "DELETE /attachments/:attachment_id",
	"PUT /attachments/:attachment_id/preview", "GET /attachments/:attachment_id/preview",
}

var collectionPaths = []string{"entries", "patterns", "activities", "places", "bookmark-folders", "bookmarks"}

func TestRouterRegistersRoutes(t *testing.T) {
	got := routes(newTestRouter(t, handlers.ModeCloud))

	want := append([]string{
		"GET /health", "POST /feedback",
		"POST /users/register", "POST /users/login/params", "POST /users/login",
		"POST /users/recover/start", "POST /users/recover/finish",
		"POST /users/erase/request", "POST /users/erase/confirm",
	}, protectedRoutes...)
	for _, col := range collectionPaths {
		want = append(want, "PUT /"+col+"/:id", "DELETE /"+col+"/:id")
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
	for _, col := range collectionPaths {
		all = append(all, "PUT /"+col+"/:id", "DELETE /"+col+"/:id")
	}
	id := "33333333-3333-3333-3333-333333333333"
	for _, route := range all {
		method, path, _ := strings.Cut(route, " ")
		path = strings.NewReplacer(":attachment_id", id, ":session_id", id, ":id", id).Replace(path)
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

func TestOpenAPIMatchesRouter(t *testing.T) {
	raw, err := os.ReadFile("../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			PathItems map[string]map[string]any `yaml:"pathItems"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for path, item := range spec.Paths {
		if ref, ok := item["$ref"].(string); ok {
			item = spec.Components.PathItems[strings.TrimPrefix(ref, "#/components/pathItems/")]
		}
		for method := range item {
			if method != "parameters" && method != "$ref" {
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
	}

	param := regexp.MustCompile(`:(\w+)`)
	var served []string
	for _, route := range routes(newTestRouter(t, handlers.ModeCloud)) {
		served = append(served, param.ReplaceAllString(route, "{$1}"))
	}
	slices.Sort(documented)
	slices.Sort(served)
	if !slices.Equal(documented, served) {
		t.Errorf("openapi.yaml routes =\n%v\nrouter routes =\n%v", documented, served)
	}
}
