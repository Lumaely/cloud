package services

import (
	"context"
	"fmt"
	"log"
)

type Email struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends transactional emails; the provider is chosen later (MAIL_DRIVER).
type Mailer interface {
	Send(ctx context.Context, email Email) error
}

// LogMailer writes emails to the server log instead of sending them.
type LogMailer struct {
	From   string
	Logger *log.Logger
}

func (m *LogMailer) Send(_ context.Context, e Email) error {
	logger := m.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("mail from=%q to=%q subject=%q\n%s", m.From, e.To, e.Subject, e.Body)
	return nil
}

type MailConfig struct {
	Driver, From, SecretKey, ProjectID, Region string
}

func NewMailer(cfg MailConfig) (Mailer, error) {
	switch cfg.Driver {
	case "", "log":
		return &LogMailer{From: cfg.From}, nil
	case "scaleway":
		if cfg.SecretKey == "" || cfg.ProjectID == "" {
			return nil, fmt.Errorf("MAIL_DRIVER scaleway needs SCW_SECRET_KEY and SCW_PROJECT_ID")
		}
		return &ScalewayMailer{From: cfg.From, SecretKey: cfg.SecretKey, ProjectID: cfg.ProjectID, Region: cfg.Region}, nil
	default:
		return nil, fmt.Errorf("unknown MAIL_DRIVER %q", cfg.Driver)
	}
}
