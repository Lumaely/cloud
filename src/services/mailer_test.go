package services

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
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
		if m, err := NewMailer(MailConfig{Driver: driver, From: "x"}); err != nil || m == nil {
			t.Errorf("NewMailer(%q) = %v, %v", driver, m, err)
		}
	}
	if _, err := NewMailer(MailConfig{Driver: "smtp", From: "x"}); err == nil {
		t.Error("unknown driver accepted")
	}
}

func TestNewMailerScalewayNeedsKeys(t *testing.T) {
	for _, cfg := range []MailConfig{
		{Driver: "scaleway", ProjectID: "p"},
		{Driver: "scaleway", SecretKey: "k"},
	} {
		if _, err := NewMailer(cfg); err == nil {
			t.Errorf("NewMailer(%+v) accepted", cfg)
		}
	}
	if m, err := NewMailer(MailConfig{Driver: "scaleway", From: "Lumaely <no-reply@test>", SecretKey: "k", ProjectID: "p"}); err != nil || m == nil {
		t.Errorf("valid config: %v, %v", m, err)
	}
}

func TestNewMailerScalewayNeedsValidFrom(t *testing.T) {
	for _, from := range []string{"", "not an address", "Lumaely <>"} {
		if _, err := NewMailer(MailConfig{Driver: "scaleway", From: from, SecretKey: "k", ProjectID: "p"}); err == nil {
			t.Errorf("sender %q accepted", from)
		}
	}
}

func TestScalewayMailerSends(t *testing.T) {
	var got map[string]any
	var method, path, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, token = r.Method, r.URL.Path, r.Header.Get("X-Auth-Token")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	m := &ScalewayMailer{From: "Lumaely <no-reply@x.test>", SecretKey: "sk", ProjectID: "proj", BaseURL: srv.URL}

	err := m.Send(context.Background(), Email{To: "a@b.c", Subject: "Code", Body: "123456"})

	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if method != http.MethodPost || path != "/transactional-email/v1alpha1/regions/fr-par/emails" || token != "sk" {
		t.Errorf("request = %s %s token %q", method, path, token)
	}
	from := got["from"].(map[string]any)
	to := got["to"].([]any)[0].(map[string]any)
	if from["email"] != "no-reply@x.test" || from["name"] != "Lumaely" || to["email"] != "a@b.c" ||
		got["subject"] != "Code" || got["text"] != "123456" || got["project_id"] != "proj" {
		t.Errorf("body = %v", got)
	}
}

func TestScalewayMailerFailsOnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := &ScalewayMailer{From: "no-reply@x.test", SecretKey: "sk", ProjectID: "p", BaseURL: srv.URL}

	err := m.Send(context.Background(), Email{To: "a@b.c", Subject: "s", Body: "b"})

	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want status 500", err)
	}
}
