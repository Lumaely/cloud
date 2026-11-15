package handlers

import (
	"cloud/src/models"
	"cloud/src/services"
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// noticeAddress returns the account email, empty outside cloud mode or without an email.
func (h *Handler) noticeAddress(ctx context.Context, userID string) string {
	if h.Settings.Mode != ModeCloud {
		return ""
	}
	var email sql.NullString
	// Runs after the response; the client may already be gone.
	ctx = context.WithoutCancel(ctx)
	if err := h.DB.QueryRowContext(ctx, `SELECT email FROM users WHERE user_id = $1`, userID).Scan(&email); err != nil {
		log.Printf("notice address lookup failed: %v", err)
		return ""
	}
	return email.String
}

func utc(at time.Time) string {
	return at.UTC().Format("2006-01-02 15:04") + " UTC"
}

// deviceLabel omits a blank name or platform.
func deviceLabel(d *models.Device) string {
	if d == nil {
		return ""
	}
	name, platform := strings.TrimSpace(d.Name), strings.TrimSpace(d.Platform)
	if name != "" && platform != "" {
		return name + " (" + platform + ")"
	}
	return name + platform
}

func noticeData(d *models.Device) services.EmailData {
	return services.EmailData{Time: utc(time.Now()), Device: deviceLabel(d)}
}
