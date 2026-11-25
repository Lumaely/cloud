package test

import (
	"context"
	"testing"

	"cloud/src/handlers"
)

func TestIntegrationAccountPruneEmailCodes(t *testing.T) {
	s := newStack(t, 0)
	rows := map[string]string{
		"stale":      `NOW() - INTERVAL '1 hour', NOW() - INTERVAL '25 hours', NOW() - INTERVAL '25 hours', 3`,
		"never-sent": `NOW() - INTERVAL '1 hour', NULL, NOW() - INTERVAL '25 hours', 0`,
		"live-code":  `NOW() + INTERVAL '1 hour', NOW() - INTERVAL '25 hours', NOW() - INTERVAL '25 hours', 1`,
		"throttled":  `NOW() - INTERVAL '1 second', NOW() - INTERVAL '30 seconds', NOW() - INTERVAL '25 hours', 1`,
		"capped":     `NOW() - INTERVAL '1 hour', NOW() - INTERVAL '2 hours', NOW() - INTERVAL '23 hours', 10`,
		"counted":    `NOW() - INTERVAL '1 hour', NOW() - INTERVAL '2 hours', NOW() - INTERVAL '23 hours', 1`,
	}
	for email, values := range rows {
		if _, err := s.db.Exec(`INSERT INTO email_codes (email, kind, expires_at, sent_at, window_start, window_count)
			VALUES ($1, 'erase', `+values+`)`, email); err != nil {
			t.Fatal(err)
		}
	}

	n, err := handlers.PruneEmailCodes(context.Background(), s.db)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v; want 2", n, err)
	}
	for email := range rows {
		var left bool
		if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM email_codes WHERE email = $1)`, email).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if want := email != "stale" && email != "never-sent"; left != want {
			t.Errorf("%s kept = %v, want %v", email, left, want)
		}
	}
}
