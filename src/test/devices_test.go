package test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud/src/models"
)

type sessionView struct {
	SessionID  string    `json:"session_id"`
	DeviceName *string   `json:"device_name"`
	Platform   *string   `json:"platform"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Current    bool      `json:"current"`
}

func (s *stack) sessions(token string) []sessionView {
	s.t.Helper()
	w := s.do(http.MethodGet, "/users/me/sessions", token, nil)
	expect(s.t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), "token") {
		s.t.Errorf("session list leaks a token field: %s", w.Body.String())
	}
	return decode[[]sessionView](s.t, w)
}

func (s *stack) loginOn(a *account, name, platform string) string {
	s.t.Helper()
	w := s.do(http.MethodPost, "/users/login", "", map[string]any{
		"email": a.email, "auth_key": a.authKey, "device": map[string]string{"name": name, "platform": platform},
	})
	expect(s.t, w, http.StatusOK)
	return decode[models.AuthResponse](s.t, w).Token
}

func (s *stack) setLastSeen(sessionID, ago string) {
	s.t.Helper()
	if _, err := s.db.Exec(`UPDATE user_sessions SET last_seen_at = NOW() - $2::interval WHERE session_id = $1`, sessionID, ago); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stack) lastSeen(sessionID string) time.Time {
	s.t.Helper()
	var at time.Time
	if err := s.db.QueryRow(`SELECT last_seen_at FROM user_sessions WHERE session_id = $1`, sessionID).Scan(&at); err != nil {
		s.t.Fatal(err)
	}
	return at
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestIntegrationDeviceList(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("dev@test.local")
	phone := s.loginOn(a, "Alice's iPhone", "ios")
	laptop := s.loginOn(a, "Work laptop", "macos")

	list := s.sessions(phone)
	if len(list) != 3 {
		t.Fatalf("sessions = %+v", list)
	}
	byName := map[string]sessionView{}
	for _, v := range list {
		byName[deref(v.DeviceName)] = v
	}
	mine := byName["Alice's iPhone"]
	if !mine.Current || deref(mine.Platform) != "ios" || mine.CreatedAt.IsZero() || mine.LastSeenAt.IsZero() {
		t.Errorf("phone session = %+v", mine)
	}
	if byName["Work laptop"].Current || deref(byName["Work laptop"].Platform) != "macos" {
		t.Errorf("laptop session = %+v", byName["Work laptop"])
	}
	if reg := byName["<nil>"]; reg.SessionID == "" || reg.Platform != nil || reg.Current {
		t.Errorf("register session = %+v", reg)
	}

	s.setLastSeen(mine.SessionID, "1 hour")
	s.setLastSeen(byName["<nil>"].SessionID, "2 hours")
	s.setLastSeen(byName["Work laptop"].SessionID, "1 minute")
	order := s.sessions(laptop)
	if deref(order[0].DeviceName) != "Work laptop" || deref(order[1].DeviceName) != "Alice's iPhone" || order[2].DeviceName != nil {
		t.Errorf("order = %+v", order)
	}
	if !order[0].Current || order[1].Current {
		t.Errorf("current flag follows caller: %+v", order)
	}

	if _, err := s.db.Exec(`UPDATE user_sessions SET expires_at = NOW() - INTERVAL '1 minute' WHERE session_id = $1`, byName["<nil>"].SessionID); err != nil {
		t.Fatal(err)
	}
	if got := s.sessions(phone); len(got) != 2 {
		t.Errorf("expired session listed: %+v", got)
	}
}

func TestIntegrationDeviceStoredOnEverySessionRoute(t *testing.T) {
	s := newStack(t, 0)
	device := map[string]string{"name": "Pixel", "platform": "android"}
	check := func(token string) {
		t.Helper()
		list := s.sessions(token)
		if len(list) != 1 || deref(list[0].DeviceName) != "Pixel" || deref(list[0].Platform) != "android" {
			t.Errorf("sessions = %+v", list)
		}
	}

	a := &account{email: "every@test.local", authKey: randomBytes(t, 32), recoveryKey: randomBytes(t, 32)}
	body := s.keyFields(a)
	body["device"] = device
	w := s.do(http.MethodPost, "/users/register", "", body)
	expect(t, w, http.StatusCreated)
	check(decode[models.AuthResponse](t, w).Token)

	w = s.do(http.MethodPost, "/users/recover/start", "", map[string]any{"email": a.email, "recovery_auth_key": a.recoveryKey})
	expect(t, w, http.StatusOK)
	w = s.do(http.MethodPost, "/users/recover/finish", "", map[string]any{
		"recovery_token": decode[models.RecoverStartResponse](t, w).RecoveryToken, "auth_key": a.authKey,
		"kdf_salt": randomBytes(t, 16), "kdf_params": map[string]any{"alg": "argon2id"},
		"wrapped_key_password": []byte("rewrapped"), "device": device,
	})
	expect(t, w, http.StatusOK)
	check(decode[models.AuthResponse](t, w).Token)

	expect(t, s.do(http.MethodPost, "/users/erase/request", "", map[string]string{"email": a.email}), http.StatusAccepted)
	confirm := s.keyFields(a)
	confirm["code"], confirm["device"] = s.mailer.lastCode(t), device
	w = s.do(http.MethodPost, "/users/erase/confirm", "", confirm)
	expect(t, w, http.StatusOK)
	check(decode[models.AuthResponse](t, w).Token)
}

func TestIntegrationDeviceFieldsLimitedTo64Characters(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("long@test.local")
	login := func(name, platform string) int {
		return s.do(http.MethodPost, "/users/login", "", map[string]any{
			"email": a.email, "auth_key": a.authKey, "device": map[string]string{"name": name, "platform": platform},
		}).Code
	}
	if got := login(strings.Repeat("é", 64), "ios"); got != http.StatusOK {
		t.Errorf("64-character name = %d, want 200", got)
	}
	if got := login(strings.Repeat("a", 65), "ios"); got != http.StatusBadRequest {
		t.Errorf("65-character name = %d, want 400", got)
	}
	if got := login("phone", strings.Repeat("a", 65)); got != http.StatusBadRequest {
		t.Errorf("65-character platform = %d, want 400", got)
	}
	reg := s.keyFields(&account{email: "long2@test.local", authKey: randomBytes(t, 32), recoveryKey: randomBytes(t, 32)})
	reg["device"] = map[string]string{"name": strings.Repeat("a", 65)}
	expect(t, s.do(http.MethodPost, "/users/register", "", reg), http.StatusBadRequest)
}

func TestIntegrationLastSeenRefreshedAtMostEveryFiveMinutes(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("seen@test.local")
	id := s.sessions(a.token)[0].SessionID

	s.setLastSeen(id, "10 minutes")
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusOK)
	if age := time.Since(s.lastSeen(id)); age > time.Minute {
		t.Errorf("stale session not refreshed, last seen %v ago", age)
	}

	s.setLastSeen(id, "4 minutes")
	before := s.lastSeen(id)
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusOK)
	if after := s.lastSeen(id); !after.Equal(before) {
		t.Errorf("recent session rewritten: %v then %v", before, after)
	}
}

func TestIntegrationSignOutOneDevice(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("one@test.local")
	phone := s.loginOn(a, "Phone", "ios")
	bob := s.register("bob@test.local")

	var phoneID, ownID string
	for _, v := range s.sessions(a.token) {
		if v.Current {
			ownID = v.SessionID
		} else {
			phoneID = v.SessionID
		}
	}
	bobID := s.sessions(bob.token)[0].SessionID

	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+bobID, a.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodGet, "/users/me", bob.token, nil), http.StatusOK)
	expect(t, s.do(http.MethodDelete, "/users/me/sessions/33333333-3333-3333-3333-333333333333", a.token, nil), http.StatusNotFound)
	expect(t, s.do(http.MethodDelete, "/users/me/sessions/nope", a.token, nil), http.StatusBadRequest)

	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+phoneID, a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodGet, "/users/me", phone, nil), http.StatusUnauthorized)
	if list := s.sessions(a.token); len(list) != 1 || list[0].SessionID != ownID {
		t.Errorf("sessions after sign-out = %+v", list)
	}
	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+phoneID, a.token, nil), http.StatusNotFound)

	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+ownID, a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusUnauthorized)
}

func TestIntegrationPreviewsOutsideQuota(t *testing.T) {
	const capBytes = 256 << 10
	s := newStack(t, capBytes+5)
	a := s.register("prev@test.local")

	pic := file{name: "pic", mediaType: "image/png", content: "12345", preview: "pppp"}
	entry := s.createEntry(a.token, "p1", pic)
	if att := entry.Attachments[0]; att.SizeBytes != 5 || att.PreviewSizeBytes != 4 {
		t.Errorf("attachment = %+v", att)
	}
	larger := file{name: "larger", mediaType: "image/png", content: "1", preview: "22"}
	if res := s.push(a.token, []map[string]any{newEntry("l", "p4", larger)}, larger); res[0].Status != "invalid" {
		t.Errorf("preview larger than its file = %+v", res[0])
	}

	max := file{name: "max", mediaType: "image/png", content: strings.Repeat("f", capBytes), preview: strings.Repeat("p", capBytes)}
	if res := s.push(a.token, []map[string]any{newEntry("m", "p2", max)}, max); res[0].Status != "created" {
		t.Errorf("256 KiB preview = %+v", res[0])
	}
	usage := decode[models.StorageUsage](t, s.do(http.MethodGet, "/users/me/storage", a.token, nil))
	if usage.UsedBytes != capBytes+5 {
		t.Errorf("used = %d, want %d", usage.UsedBytes, capBytes+5)
	}

	over := file{name: "over", mediaType: "image/png", content: strings.Repeat("f", capBytes+1), preview: strings.Repeat("p", capBytes+1)}
	if res := s.push(a.token, []map[string]any{newEntry("o", "p3", over)}, over); res[0].Status != "invalid" {
		t.Errorf("preview over 256 KiB = %+v", res[0])
	}
	if keys := s.objectKeys(a.userID + "/"); len(keys) != 4 {
		t.Errorf("objects = %v, want 4", keys)
	}
}

func TestIntegrationURNUUIDsAccepted(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("urn@test.local")
	other := s.loginOn(a, "Other", "ios")
	urn := func(id string) string { return "urn:uuid:" + id }

	entry := s.createEntry(a.token, "u1", file{name: "f", mediaType: "image/png", content: "x"})
	expect(t, s.do(http.MethodGet, "/attachments/"+urn(entry.Attachments[0].AttachmentID), a.token, nil), http.StatusOK)

	rec := decode[models.Record](t, s.do(http.MethodPost, "/patterns", a.token, map[string]any{"blob": []byte("p")}))
	expect(t, s.do(http.MethodPut, "/patterns/"+urn(rec.ID), a.token, map[string]any{"blob": []byte("q"), "version": 1}), http.StatusOK)
	expect(t, s.do(http.MethodPost, "/bookmarks", a.token, map[string]any{"blob": []byte("b"), "entry_id": urn(entry.EntryID)}), http.StatusCreated)

	update := newEntry("u", "u2")
	update["entry_id"], update["version"] = urn(entry.EntryID), entry.Version
	if res := s.push(a.token, []map[string]any{update}); res[0].Status != "updated" {
		t.Errorf("sync update by urn = %+v", res[0])
	}

	var otherID string
	for _, v := range s.sessions(a.token) {
		if !v.Current {
			otherID = v.SessionID
		}
	}
	expect(t, s.do(http.MethodDelete, "/users/me/sessions/"+urn(otherID), a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodGet, "/users/me", other, nil), http.StatusUnauthorized)
	expect(t, s.do(http.MethodDelete, "/attachments/"+urn(entry.Attachments[0].AttachmentID), a.token, nil), http.StatusNoContent)
	expect(t, s.do(http.MethodDelete, "/patterns/"+urn(rec.ID), a.token, nil), http.StatusNoContent)
}

func TestIntegrationSignInPrunesExpiredAndStalestSessions(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("cap@test.local")
	_, err := s.db.Exec(`
		INSERT INTO user_sessions (user_id, token_hash, expires_at, last_seen_at, device_name)
		SELECT $1, md5(i::text), NOW() + INTERVAL '1 day', NOW() - i * INTERVAL '1 minute', 'seed-' || i
		FROM generate_series(1, 50) AS i
	`, a.userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user_sessions (user_id, token_hash, expires_at) VALUES ($1, 'expired', NOW() - INTERVAL '1 minute')`, a.userID); err != nil {
		t.Fatal(err)
	}

	token := s.loginOn(a, "New", "ios")

	var rows, expired int
	var stalest string
	err = s.db.QueryRow(`
		SELECT count(*), count(*) FILTER (WHERE expires_at <= NOW()),
			(SELECT device_name FROM user_sessions WHERE user_id = $1 ORDER BY last_seen_at ASC LIMIT 1)
		FROM user_sessions WHERE user_id = $1
	`, a.userID).Scan(&rows, &expired, &stalest)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 50 || expired != 0 || stalest != "seed-48" {
		t.Errorf("rows=%d expired=%d stalest=%q, want 50, 0, seed-48", rows, expired, stalest)
	}
	expect(t, s.do(http.MethodGet, "/users/me", token, nil), http.StatusOK)
	expect(t, s.do(http.MethodGet, "/users/me", a.token, nil), http.StatusOK)
}

func TestIntegrationSignInNeverPrunesItsOwnSession(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("self@test.local")
	_, err := s.db.Exec(`
		INSERT INTO user_sessions (user_id, token_hash, expires_at, last_seen_at)
		SELECT $1, md5(i::text), NOW() + INTERVAL '1 day', NOW() + INTERVAL '1 minute'
		FROM generate_series(1, 50) AS i
	`, a.userID)
	if err != nil {
		t.Fatal(err)
	}

	token := s.loginOn(a, "New", "ios")

	expect(t, s.do(http.MethodGet, "/users/me", token, nil), http.StatusOK)
}
