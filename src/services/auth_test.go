package services

import (
	"encoding/hex"
	"testing"
)

func TestHashPasswordAndCheckPassword(t *testing.T) {
	s := NewAuthService()

	hash, err := s.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "s3cret" {
		t.Fatal("hash equals plaintext")
	}
	if !s.CheckPassword("s3cret", hash) {
		t.Error("valid password rejected")
	}
	if s.CheckPassword("wrong", hash) {
		t.Error("invalid password accepted")
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	s := NewAuthService()
	a, _ := s.HashPassword("same")
	b, _ := s.HashPassword("same")
	if a == b {
		t.Error("two hashes of the same password are equal")
	}
}

func TestCheckPasswordRejectsMalformedHash(t *testing.T) {
	if NewAuthService().CheckPassword("pw", "not-a-bcrypt-hash") {
		t.Error("malformed hash accepted")
	}
}

func TestGenerateSessionToken(t *testing.T) {
	s := NewAuthService()

	raw, hash, err := s.GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken: %v", err)
	}
	if len(raw) != 64 {
		t.Errorf("raw token length = %d, want 64", len(raw))
	}
	if _, err := hex.DecodeString(raw); err != nil {
		t.Errorf("raw token not hex: %v", err)
	}
	if hash != s.HashToken(raw) {
		t.Error("returned hash does not match HashToken(raw)")
	}

	raw2, _, _ := s.GenerateSessionToken()
	if raw == raw2 {
		t.Error("two tokens are equal")
	}
}

func TestHashToken(t *testing.T) {
	s := NewAuthService()
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := s.HashToken("hello"); got != want {
		t.Errorf("HashToken(hello) = %s, want %s", got, want)
	}
}
