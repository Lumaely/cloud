package services

import (
	_ "embed"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// EmailKind names one entry of emails.yaml.
type EmailKind string

const (
	EmailSignupCode       EmailKind = "signup_code"
	EmailAccountExists    EmailKind = "account_exists"
	EmailEraseCode        EmailKind = "erase_code"
	EmailSignIn           EmailKind = "sign_in"
	EmailPasswordChanged  EmailKind = "password_changed"
	EmailRecoveryReplaced EmailKind = "recovery_replaced"
	EmailRecoveryUsed     EmailKind = "recovery_used"
	EmailSignedOutAll     EmailKind = "signed_out_all"
	EmailDiaryReset       EmailKind = "diary_reset"
)

var emailKinds = []EmailKind{
	EmailSignupCode, EmailAccountExists, EmailEraseCode, EmailSignIn, EmailPasswordChanged,
	EmailRecoveryReplaced, EmailRecoveryUsed, EmailSignedOutAll, EmailDiaryReset,
}

// EmailData fills the templates; Time and Device are preformatted by the caller.
type EmailData struct {
	Code            string
	Time            string
	Device          string
	SignedOutOthers bool
}

//go:embed emails.yaml
var emailsYAML []byte

// EmailCatalog holds the sender and the parsed templates of every email; emails.yaml is its only source.
type EmailCatalog struct {
	From      string
	templates map[EmailKind]emailTemplate
}

type emailTemplate struct {
	subject, body *template.Template
}

func LoadEmailCatalog() (*EmailCatalog, error) {
	return parseEmailCatalog(emailsYAML)
}

func parseEmailCatalog(raw []byte) (*EmailCatalog, error) {
	var file struct {
		From   string `yaml:"from"`
		Emails map[string]struct {
			Subject string `yaml:"subject"`
			Body    string `yaml:"body"`
		} `yaml:"emails"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("emails.yaml: %w", err)
	}
	if _, err := mail.ParseAddress(file.From); err != nil {
		return nil, fmt.Errorf("emails.yaml: invalid from %q: %w", file.From, err)
	}

	c := &EmailCatalog{From: file.From, templates: map[EmailKind]emailTemplate{}}
	for _, kind := range emailKinds {
		e, ok := file.Emails[string(kind)]
		if !ok {
			return nil, fmt.Errorf("emails.yaml: missing email %q", kind)
		}
		if strings.TrimSpace(e.Subject) == "" || strings.TrimSpace(e.Body) == "" {
			return nil, fmt.Errorf("emails.yaml: %q needs a subject and a body", kind)
		}
		subject, err := parseEmailTemplate(kind, "subject", e.Subject)
		if err != nil {
			return nil, err
		}
		body, err := parseEmailTemplate(kind, "body", e.Body)
		if err != nil {
			return nil, err
		}
		c.templates[kind] = emailTemplate{subject: subject, body: body}
	}
	for name := range file.Emails {
		if _, ok := c.templates[EmailKind(name)]; !ok {
			return nil, fmt.Errorf("emails.yaml: unknown email %q", name)
		}
	}
	return c, nil
}

// parseEmailTemplate also executes the template once, so an unknown field fails at load, not on send.
func parseEmailTemplate(kind EmailKind, part, text string) (*template.Template, error) {
	t, err := template.New(string(kind) + "." + part).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("emails.yaml: %w", err)
	}
	if err := t.Execute(&strings.Builder{}, EmailData{}); err != nil {
		return nil, fmt.Errorf("emails.yaml: %w", err)
	}
	return t, nil
}

func (c *EmailCatalog) Render(kind EmailKind, to string, data EmailData) (Email, error) {
	t, ok := c.templates[kind]
	if !ok {
		return Email{}, errors.New("unknown email " + string(kind))
	}
	var subject, body strings.Builder
	if err := t.subject.Execute(&subject, data); err != nil {
		return Email{}, err
	}
	if err := t.body.Execute(&body, data); err != nil {
		return Email{}, err
	}
	return Email{To: to, Subject: subject.String(), Body: body.String()}, nil
}
