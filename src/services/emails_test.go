package services

import (
	"strings"
	"testing"
)

func TestEmbeddedCatalogRendersEveryKind(t *testing.T) {
	c, err := LoadEmailCatalog()
	if err != nil {
		t.Fatalf("LoadEmailCatalog: %v", err)
	}
	if c.From == "" {
		t.Error("empty sender")
	}
	data := EmailData{Code: "482913", Time: "2026-01-02 03:04 UTC", Device: "Mac (macos)", SignedOutOthers: true}
	for _, kind := range emailKinds {
		e, err := c.Render(kind, "a@b.c", data)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if e.To != "a@b.c" || strings.TrimSpace(e.Subject) == "" || strings.TrimSpace(e.Body) == "" {
			t.Errorf("%s = %+v", kind, e)
		}
	}
	for _, kind := range []EmailKind{EmailSignupCode, EmailEraseCode} {
		if e, _ := c.Render(kind, "a@b.c", data); !strings.Contains(e.Body, "482913") {
			t.Errorf("%s body misses the code: %q", kind, e.Body)
		}
	}
}

func TestRenderUnknownKindFails(t *testing.T) {
	c, err := LoadEmailCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Render("nope", "a@b.c", EmailData{}); err == nil {
		t.Error("unknown kind rendered")
	}
}

func catalogYAML(from string, override map[EmailKind]string, extra string) []byte {
	var b strings.Builder
	b.WriteString("from: \"" + from + "\"\nemails:\n")
	for _, kind := range emailKinds {
		entry, ok := override[kind]
		if !ok {
			entry = "    subject: \"s\"\n    body: \"b {{.Code}}\"\n"
		}
		if entry == "-" {
			continue
		}
		b.WriteString("  " + string(kind) + ":\n" + entry)
	}
	b.WriteString(extra)
	return []byte(b.String())
}

func TestParseEmailCatalog(t *testing.T) {
	if _, err := parseEmailCatalog(catalogYAML("Lumaely <a@b.c>", nil, "")); err != nil {
		t.Fatalf("valid catalog: %v", err)
	}
	for name, raw := range map[string][]byte{
		"invalid yaml":          []byte("from: [\n"),
		"missing entry":         catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "-"}, ""),
		"unknown entry":         catalogYAML("Lumaely <a@b.c>", nil, "  welcome:\n    subject: \"s\"\n    body: \"b\"\n"),
		"invalid from":          catalogYAML("not an address", nil, ""),
		"empty from":            catalogYAML("", nil, ""),
		"empty subject":         catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "    subject: \"\"\n    body: \"b\"\n"}, ""),
		"empty body":            catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "    subject: \"s\"\n    body: \" \"\n"}, ""),
		"bad template":          catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "    subject: \"s\"\n    body: \"{{.Code\"\n"}, ""),
		"unknown field":         catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "    subject: \"{{.Name}}\"\n    body: \"b\"\n"}, ""),
		"unknown field in if":   catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailSignIn: "    subject: \"s\"\n    body: \"{{if .Device}}{{.Devise}}{{end}}\"\n"}, ""),
		"unknown field in else": catalogYAML("Lumaely <a@b.c>", map[EmailKind]string{EmailPasswordChanged: "    subject: \"s\"\n    body: \"{{if .SignedOutOthers}}y{{else}}{{.Other}}{{end}}\"\n"}, ""),
	} {
		if _, err := parseEmailCatalog(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
