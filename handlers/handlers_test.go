package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthCheck(t *testing.T) {
	env := newTestEnv(t)

	w := serve(env.h.HealthCheck, httptest.NewRequest(http.MethodGet, "/health", nil))

	assertStatus(t, w, http.StatusOK)
	if got := decode[map[string]string](t, w)["status"]; got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
}
