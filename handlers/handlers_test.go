package handlers

import (
	"bytes"
	"cloud/models"
	"encoding/json"
	"errors"
	"mime/multipart"
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
	valid := models.RecoveryMaterial{
		RecoveryAuthKey:      make([]byte, keyLength),
		WrappedKeyRecovery:   []byte("w"),
		EncryptedRecoveryKey: []byte("e"),
	}
	if err := validateRecoveryMaterial(valid); err != nil {
		t.Fatalf("valid material rejected: %v", err)
	}
	for name, r := range map[string]models.RecoveryMaterial{
		"short auth key":  {RecoveryAuthKey: []byte{1}, WrappedKeyRecovery: []byte("w"), EncryptedRecoveryKey: []byte("e")},
		"missing wrapped": {RecoveryAuthKey: make([]byte, keyLength), EncryptedRecoveryKey: []byte("e")},
		"missing copy":    {RecoveryAuthKey: make([]byte, keyLength), WrappedKeyRecovery: []byte("w")},
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

func TestNonNil(t *testing.T) {
	if nonNil(nil) == nil || string(nonNil([]byte("x"))) != "x" {
		t.Error("nonNil")
	}
}

func multipartFiles(t *testing.T, names ...string) map[string][]*multipart.FileHeader {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, n := range names {
		fw, _ := mw.CreateFormFile(n, n)
		fw.Write([]byte("12345"))
	}
	mw.Close()
	form, err := multipart.NewReader(&buf, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	return form.File
}

func TestValidateOperation(t *testing.T) {
	files := multipartFiles(t, "f1", "p1")
	valid := func() models.EntryOperation {
		return models.EntryOperation{
			EntryTimestamp: "2026-11-09T10:00:00Z",
			SHA256Hash:     "h",
			Attachments: []models.AttachmentOperation{
				{File: "f1", Preview: "p1", EncryptedAESKey: []byte("k")},
			},
		}
	}

	pending, _, err := validateOperation(valid(), files)
	if err != nil {
		t.Fatalf("valid operation rejected: %v", err)
	}
	if len(pending) != 1 || pending[0].size != 10 || pending[0].mediaType != "application/octet-stream" {
		t.Errorf("pending = %+v", pending)
	}

	cases := map[string]func(*models.EntryOperation){
		"bad entry_id":       func(op *models.EntryOperation) { op.EntryID = "x" },
		"update w/o version": func(op *models.EntryOperation) { op.EntryID = "22222222-2222-2222-2222-222222222222" },
		"bad timestamp":      func(op *models.EntryOperation) { op.EntryTimestamp = "today" },
		"missing hash":       func(op *models.EntryOperation) { op.SHA256Hash = "" },
		"missing file part":  func(op *models.EntryOperation) { op.Attachments[0].File = "nope" },
		"missing preview":    func(op *models.EntryOperation) { op.Attachments[0].Preview = "nope" },
		"missing aes key":    func(op *models.EntryOperation) { op.Attachments[0].EncryptedAESKey = nil },
		"more than 10 files": func(op *models.EntryOperation) { op.Attachments = make([]models.AttachmentOperation, 11) },
	}
	for name, mutate := range cases {
		op := valid()
		mutate(&op)
		_, _, err := validateOperation(op, files)
		var oe *opError
		if !errors.As(err, &oe) || oe.status != statusInvalid {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
}

func TestResourcesDeclareRoutes(t *testing.T) {
	var paths []string
	for _, r := range Resources() {
		paths = append(paths, r.Path)
	}
	if got := strings.Join(paths, ","); got != "patterns,activities,places,bookmark-folders,bookmarks" {
		t.Errorf("resources = %s", got)
	}
}
