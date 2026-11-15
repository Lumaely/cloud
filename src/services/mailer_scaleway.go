package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"time"
)

// ScalewayMailer sends emails through Scaleway Transactional Email.
type ScalewayMailer struct {
	From, SecretKey, ProjectID, Region, BaseURL string
	Client                                      *http.Client
}

type scalewayAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type scalewayEmail struct {
	From      scalewayAddress   `json:"from"`
	To        []scalewayAddress `json:"to"`
	Subject   string            `json:"subject"`
	Text      string            `json:"text"`
	ProjectID string            `json:"project_id"`
}

func (m *ScalewayMailer) Send(ctx context.Context, e Email) error {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("invalid sender: %w", err)
	}
	body, err := json.Marshal(scalewayEmail{
		From:      scalewayAddress{Email: from.Address, Name: from.Name},
		To:        []scalewayAddress{{Email: e.To}},
		Subject:   e.Subject,
		Text:      e.Body,
		ProjectID: m.ProjectID,
	})
	if err != nil {
		return err
	}

	baseURL, region, client := m.BaseURL, m.Region, m.Client
	if baseURL == "" {
		baseURL = "https://api.scaleway.com"
	}
	if region == "" {
		region = "fr-par"
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	url := fmt.Sprintf("%s/transactional-email/v1alpha1/regions/%s/emails", baseURL, region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", m.SecretKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("scaleway mail: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("scaleway mail: status %d: %s", resp.StatusCode, detail)
	}
	return nil
}
