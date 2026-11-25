package test

import (
	"net/http"
	"strings"
	"testing"

	"cloud/src/handlers"
	"cloud/src/models"
)

func (s *stack) onlyNotice(subject string) string {
	s.t.Helper()
	s.h.WaitNotices()
	got := s.mailer.sent()
	if len(got) != 1 || got[0].Subject != subject || got[0].To != "alice@test.local" {
		s.t.Fatalf("emails = %+v, want one %q", got, subject)
	}
	s.mailer.reset()
	return got[0].Body
}

func (s *stack) noNotice() {
	s.t.Helper()
	s.h.WaitNotices()
	if n := s.mailer.count(); n != 0 {
		s.t.Fatalf("sent %d emails, want none: %+v", n, s.mailer.sent())
	}
}

func (s *stack) passwordBody(a *account, extra map[string]any) map[string]any {
	body := map[string]any{
		"current_auth_key": a.authKey, "auth_key": a.authKey,
		"kdf_salt": randomBytes(s.t, 16), "kdf_params": map[string]any{"alg": "argon2id"},
		"wrapped_key_password": []byte("rewrapped"),
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestIntegrationNoticeSignIn(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")
	expect(t, s.do(http.MethodPost, "/users/login", "", map[string]any{
		"email": a.email, "auth_key": a.authKey, "device": map[string]string{"name": "Pixel 9", "platform": "android"},
	}), http.StatusOK)
	body := s.onlyNotice("New sign-in to your Lumaely account")
	for _, want := range []string{"Pixel 9", "android", "UTC", "change your password"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}

	expect(t, s.login(a.email, randomBytes(t, 32)), http.StatusUnauthorized)
	s.noNotice()

	expect(t, s.login(a.email, a.authKey), http.StatusOK)
	s.onlyNotice("New sign-in to your Lumaely account")
}

func TestIntegrationNoticePasswordChange(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")

	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, s.passwordBody(a, nil)), http.StatusNoContent)
	body := s.onlyNotice("Your Lumaely password was changed")
	if strings.Contains(body, "signed out") && !strings.Contains(body, "not signed out") {
		t.Errorf("claims sign-out: %s", body)
	}

	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, s.passwordBody(a, map[string]any{"sign_out_other_devices": true})), http.StatusNoContent)
	body = s.onlyNotice("Your Lumaely password was changed")
	if !strings.Contains(body, "signed out") || strings.Contains(body, "not signed out") {
		t.Errorf("no sign-out mention: %s", body)
	}

	rec := map[string]any{"recovery_auth_key": randomBytes(t, 32), "wrapped_key_recovery": []byte("new-recovery")}
	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, s.passwordBody(a, map[string]any{"recovery": rec})), http.StatusNoContent)
	s.onlyNotice("Your Lumaely recovery key was replaced")

	wrong := s.passwordBody(a, nil)
	wrong["current_auth_key"] = randomBytes(t, 32)
	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, wrong), http.StatusUnauthorized)
	s.noNotice()
}

func TestIntegrationNoticeRecoveryUsed(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")
	w := s.do(http.MethodPost, "/users/recover/start", "", map[string]any{"email": a.email, "recovery_auth_key": a.recoveryKey})
	expect(t, w, http.StatusOK)
	s.noNotice()
	expect(t, s.do(http.MethodPost, "/users/recover/finish", "", map[string]any{
		"recovery_token": decode[models.RecoverStartResponse](t, w).RecoveryToken, "auth_key": a.authKey,
		"kdf_salt": randomBytes(t, 16), "kdf_params": map[string]any{"alg": "argon2id"},
		"wrapped_key_password": []byte("rewrapped"), "device": map[string]string{"name": "Mac", "platform": "macos"},
	}), http.StatusOK)
	body := s.onlyNotice("Your Lumaely recovery key was used")
	if !strings.Contains(body, "Mac") {
		t.Errorf("body missing device: %s", body)
	}
}

func TestIntegrationNoticeSignOutAllAndDiaryReset(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")
	expect(t, s.do(http.MethodDelete, "/users/me/diary", a.token, nil), http.StatusNoContent)
	s.onlyNotice("Your Lumaely diary was erased")
	expect(t, s.do(http.MethodDelete, "/users/me/sessions", a.token, nil), http.StatusNoContent)
	s.onlyNotice("All your Lumaely devices were signed out")
}

func TestIntegrationNoticeSkippedForSingleSessionOrErase(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")
	var id string
	s.db.QueryRow(`SELECT session_id FROM user_sessions WHERE user_id = $1`, a.userID).Scan(&id)
	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+id, a.token, nil), http.StatusNoContent)
	s.noNotice()

	b := s.register("bob@test.local")
	s.requestErase(b.email)
	confirm := s.keyFields(b)
	confirm["code"] = s.mailer.lastCode(t)
	s.mailer.reset()
	expect(t, s.do(http.MethodPost, "/users/erase/confirm", "", confirm), http.StatusOK)
	s.noNotice()
}

func TestIntegrationNoticeFailSoftAndSelfHosted(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("alice@test.local")
	s.mailer.setFail(true)
	expect(t, s.login(a.email, a.authKey), http.StatusOK)
	expect(t, s.do(http.MethodPut, "/users/me/password", a.token, s.passwordBody(a, nil)), http.StatusNoContent)
	expect(t, s.do(http.MethodDelete, "/users/me/diary", a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodDelete, "/users/me/sessions", a.token, nil), http.StatusNoContent)
	s.h.WaitNotices()

	h := newStackMode(t, 0, handlers.ModeSelfHosted)
	b := h.register("alice@test.local")
	expect(t, h.login(b.email, b.authKey), http.StatusOK)
	h.noNotice()
}
