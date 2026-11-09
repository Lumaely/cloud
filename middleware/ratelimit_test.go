package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newClockedLimiter(limit int, window time.Duration) (*RateLimiter, *time.Time) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	l := NewRateLimiter(limit, window)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestRateLimiterBlocksAfterLimit(t *testing.T) {
	l, _ := newClockedLimiter(2, time.Minute)

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("first two requests blocked")
	}
	if l.Allow("k") {
		t.Error("third request allowed")
	}
	if !l.Allow("other") {
		t.Error("other key blocked")
	}
}

func TestRateLimiterResetsAfterWindow(t *testing.T) {
	l, now := newClockedLimiter(1, time.Minute)
	l.Allow("k")

	*now = now.Add(time.Minute)

	if !l.Allow("k") {
		t.Error("request blocked in a new window")
	}
}

func TestRateLimiterSweepsExpiredWindows(t *testing.T) {
	l, now := newClockedLimiter(1, time.Minute)
	l.Allow("a")
	l.Allow("b")

	*now = now.Add(2 * time.Minute)
	l.Allow("c")

	if len(l.windows) != 1 {
		t.Errorf("windows = %d, want 1 after sweep", len(l.windows))
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	l, _ := newClockedLimiter(1, time.Minute)
	r := gin.New()
	r.GET("/x", RateLimit(l), func(c *gin.Context) { c.Status(http.StatusOK) })

	codes := make([]int, 2)
	for i := range codes {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		codes[i] = w.Code
	}

	if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
		t.Errorf("codes = %v, want [200 429]", codes)
	}
}
