package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiter is an in-memory fixed-window counter, per key; one instance per server process.
type RateLimiter struct {
	limit     int
	window    time.Duration
	now       func() time.Time
	mu        sync.Mutex
	windows   map[string]*rateWindow
	lastSweep time.Time
}

type rateWindow struct {
	start time.Time
	count int
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, now: time.Now, windows: map[string]*rateWindow{}}
}

func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if now.Sub(l.lastSweep) > l.window {
		for k, w := range l.windows {
			if now.Sub(w.start) >= l.window {
				delete(l.windows, k)
			}
		}
		l.lastSweep = now
	}

	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= l.window {
		l.windows[key] = &rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// RateLimit keys by route and client IP; set TRUSTED_PROXIES behind a reverse proxy.
func RateLimit(l *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.Allow(c.FullPath() + "|" + c.ClientIP()) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests, try again later"})
			return
		}
		c.Next()
	}
}
