package handlers

import (
	"bytes"
	"cloud/src/models"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func TestHealthCheck(t *testing.T) {
	h := &Handler{Settings: Settings{Version: "1.2.3", Mode: ModeSelfHosted}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.HealthCheck(c)

	var got map[string]string
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got["status"] != "ok" || got["version"] != "1.2.3" || got["mode"] != ModeSelfHosted {
		t.Errorf("got %d %v", w.Code, got)
	}
}

func pageContext(query string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x"+query, nil)
	return c
}

func TestParsePage(t *testing.T) {
	cases := []struct {
		query  string
		want   pageRequest
		hasErr bool
	}{
		{"", pageRequest{cursor: 0, limit: defaultPageSize}, false},
		{"?cursor=42&limit=10", pageRequest{cursor: 42, limit: 10}, false},
		{"?limit=500", pageRequest{limit: 500}, false},
		{"?cursor=-1", pageRequest{}, true},
		{"?cursor=x", pageRequest{}, true},
		{"?limit=0", pageRequest{}, true},
		{"?limit=501", pageRequest{}, true},
	}
	for _, tc := range cases {
		got, err := parsePage(pageContext(tc.query))
		if (err != nil) != tc.hasErr || (!tc.hasErr && got != tc.want) {
			t.Errorf("parsePage(%q) = %+v, %v", tc.query, got, err)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Errorf("got %q", got)
	}
}

func validKeyMaterial() models.KeyMaterial {
	return models.KeyMaterial{
		KDFSalt:            bytes.Repeat([]byte{1}, kdfSaltLength),
		KDFParams:          json.RawMessage(`{"alg":"argon2id"}`),
		WrappedKeyPassword: []byte("wrapped"),
	}
}

func TestValidateKeyMaterial(t *testing.T) {
	if err := validateKeyMaterial(validKeyMaterial()); err != nil {
		t.Fatalf("valid material rejected: %v", err)
	}
	cases := map[string]func(*models.KeyMaterial){
		"short salt":     func(k *models.KeyMaterial) { k.KDFSalt = []byte{1} },
		"params array":   func(k *models.KeyMaterial) { k.KDFParams = json.RawMessage(`[]`) },
		"params invalid": func(k *models.KeyMaterial) { k.KDFParams = json.RawMessage(`{`) },
		"empty wrapped":  func(k *models.KeyMaterial) { k.WrappedKeyPassword = nil },
		"huge wrapped":   func(k *models.KeyMaterial) { k.WrappedKeyPassword = make([]byte, maxWrappedKey+1) },
	}
	for name, mutate := range cases {
		k := validKeyMaterial()
		mutate(&k)
		if validateKeyMaterial(k) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestValidateRecoveryMaterial(t *testing.T) {
	valid := models.RecoveryMaterial{RecoveryAuthKey: make([]byte, keyLength), WrappedKeyRecovery: []byte("w")}
	if err := validateRecoveryMaterial(valid); err != nil {
		t.Fatalf("valid material rejected: %v", err)
	}
	for name, r := range map[string]models.RecoveryMaterial{
		"short auth key":  {RecoveryAuthKey: []byte{1}, WrappedKeyRecovery: []byte("w")},
		"missing wrapped": {RecoveryAuthKey: make([]byte, keyLength)},
	} {
		if validateRecoveryMaterial(r) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFakeSaltIsStablePerEmailAndSecret(t *testing.T) {
	h := &Handler{Settings: Settings{ServerSecret: []byte("secret")}}
	other := &Handler{Settings: Settings{ServerSecret: []byte("other")}}

	a := h.fakeSalt("a@b.c")
	if len(a) != kdfSaltLength || !bytes.Equal(a, h.fakeSalt("a@b.c")) {
		t.Errorf("salt %x not stable or wrong size", a)
	}
	if bytes.Equal(a, h.fakeSalt("x@b.c")) || bytes.Equal(a, other.fakeSalt("a@b.c")) {
		t.Error("salt does not depend on email and secret")
	}
}

func TestRandomDigits(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		code, err := randomDigits(6)
		if err != nil || !regexp.MustCompile(`^\d{6}$`).MatchString(code) {
			t.Fatalf("code %q, err %v", code, err)
		}
		seen[code] = true
	}
	if len(seen) < 45 {
		t.Errorf("only %d distinct codes out of 50", len(seen))
	}
}

func TestRandomHexAndHashSecret(t *testing.T) {
	tok, err := randomHex(32)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, err %v", tok, err)
	}
	if hashSecret("a") == hashSecret("b") || len(hashSecret("a")) != 64 {
		t.Error("hashSecret is not a sha256 hex digest")
	}
}

func TestFirstError(t *testing.T) {
	e := errors.New("e")
	if firstError(nil, e, errors.New("f")) != e || firstError(nil, nil) != nil {
		t.Error("firstError did not return the first non-nil error")
	}
}

func TestNewObjectKey(t *testing.T) {
	a, b := newObjectKey("user", "entry"), newObjectKey("user", "entry")
	if !regexp.MustCompile(`^user/entry/[0-9a-f]{32}$`).MatchString(a) || a == b {
		t.Errorf("keys %q %q", a, b)
	}
}

func TestValidRecordID(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	for _, id := range []string{hex64, "01890a5d-ac96-774b-bcce-b302099a8057"} {
		if !validRecordID(id) {
			t.Errorf("%q rejected", id)
		}
	}
	for _, id := range []string{
		"", "2026-10-29", strings.ToUpper(hex64), hex64[:63], hex64 + "0", "g" + hex64[1:],
		"01890A5D-AC96-774B-BCCE-B302099A8057", "urn:uuid:01890a5d-ac96-774b-bcce-b302099a8057",
		"{01890a5d-ac96-774b-bcce-b302099a8057}", "01890a5dac96774bbcceb302099a8057",
		"01890a5d-ac96-474b-bcce-b302099a8057", "01890a5d-ac96-774b-7cce-b302099a8057",
	} {
		if validRecordID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestValidateChange(t *testing.T) {
	valid := func() models.Change {
		return models.Change{Collection: "entries", ID: strings.Repeat("ab", 32), Blob: []byte("b")}
	}
	if err := validateChange(valid()); err != nil {
		t.Fatalf("valid change rejected: %v", err)
	}
	del := models.Change{Collection: "patterns", ID: "01890a5d-ac96-774b-bcce-b302099a8057", Version: 2, Deleted: true}
	if err := validateChange(del); err != nil {
		t.Fatalf("valid delete rejected: %v", err)
	}

	cases := map[string]func(*models.Change){
		"unknown collection": func(c *models.Change) { c.Collection = "moods" },
		"bad id":             func(c *models.Change) { c.ID = "x" },
		"negative version":   func(c *models.Change) { c.Version = -1 },
		"empty blob":         func(c *models.Change) { c.Blob = nil },
		"blob too large":     func(c *models.Change) { c.Blob = make([]byte, maxBlobBytes+1) },
		"delete version 0":   func(c *models.Change) { c.Deleted = true },
		"delete with blob":   func(c *models.Change) { c.Deleted, c.Version = true, 1 },
	}
	for name, mutate := range cases {
		c := valid()
		mutate(&c)
		var oe *opError
		if err := validateChange(c); !errors.As(err, &oe) || oe.status != statusInvalid {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
}

func TestCollectionsDeclareRoutes(t *testing.T) {
	if got := strings.Join(Collections, ","); got != "entries,patterns,activities,places,bookmark-folders,bookmarks" {
		t.Errorf("collections = %s", got)
	}
}
