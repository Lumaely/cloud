package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestShutdownWaitsForPending(t *testing.T) {
	waited := false
	err := Shutdown(context.Background(), &http.Server{}, func() { waited = true })
	if err != nil || !waited {
		t.Fatalf("err = %v, waited = %v", err, waited)
	}
}

func TestShutdownStopsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	block := make(chan struct{})
	defer close(block)
	err := Shutdown(ctx, &http.Server{}, func() { <-block })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
