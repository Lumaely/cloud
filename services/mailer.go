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

func NewMailer(driver, from string) (Mailer, error) {
	switch driver {
	case "", "log":
		return &LogMailer{From: from}, nil
	default:
		return nil, fmt.Errorf("unknown MAIL_DRIVER %q", driver)
	}
}
