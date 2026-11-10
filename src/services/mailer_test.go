package services

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestLogMailerWritesEmail(t *testing.T) {
	var buf bytes.Buffer
	m := &LogMailer{From: "no-reply@test", Logger: log.New(&buf, "", 0)}

	err := m.Send(context.Background(), Email{To: "a@b.c", Subject: "Code", Body: "123456"})

	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, want := range []string{"no-reply@test", "a@b.c", "Code", "123456"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q misses %q", buf.String(), want)
		}
	}
}

func TestNewMailer(t *testing.T) {
	for _, driver := range []string{"", "log"} {
		if m, err := NewMailer(driver, "x"); err != nil || m == nil {
			t.Errorf("NewMailer(%q) = %v, %v", driver, m, err)
		}
	}
	if _, err := NewMailer("smtp", "x"); err == nil {
		t.Error("unknown driver accepted")
	}
}
