package handlers

import (
	"cloud/src/models"
	"cloud/src/services"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

const ifNotYou = "If this wasn't you, change your password and sign out all devices from Settings, then check your recovery key."

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

func deviceLine(d *models.Device) string {
	if d == nil {
		return ""
	}
	name, platform := strings.TrimSpace(d.Name), strings.TrimSpace(d.Platform)
	switch {
	case name != "" && platform != "":
		return "Device: " + name + " (" + platform + ")\n"
	case name != "" || platform != "":
		return "Device: " + name + platform + "\n"
	}
	return ""
}

func noticeSignIn(to string, d *models.Device, at time.Time) services.Email {
	return services.Email{
		To:      to,
		Subject: "New sign-in to your Lumaely account",
		Body:    fmt.Sprintf("Your Lumaely account was signed in to.\nTime: %s\n%s\n%s", utc(at), deviceLine(d), ifNotYou),
	}
}

func noticePasswordChanged(to string, signedOutOthers bool, at time.Time) services.Email {
	others := "Your other devices were not signed out."
	if signedOutOthers {
		others = "Your other devices were signed out."
	}
	return services.Email{
		To:      to,
		Subject: "Your Lumaely password was changed",
		Body:    fmt.Sprintf("Your Lumaely password was changed.\nTime: %s\n%s\n\n%s", utc(at), others, ifNotYou),
	}
}

func noticeRecoveryReplaced(to string, at time.Time) services.Email {
	return services.Email{
		To:      to,
		Subject: "Your Lumaely recovery key was replaced",
		Body:    fmt.Sprintf("Your Lumaely recovery key was replaced, along with your password.\nTime: %s\n\n%s", utc(at), ifNotYou),
	}
}

func noticeRecoveryUsed(to string, d *models.Device, at time.Time) services.Email {
	return services.Email{
		To:      to,
		Subject: "Your Lumaely recovery key was used",
		Body:    fmt.Sprintf("Your recovery key was used to reset your password. All other devices were signed out.\nTime: %s\n%s\n%s", utc(at), deviceLine(d), ifNotYou),
	}
}

func noticeSignedOutAll(to string, at time.Time) services.Email {
	return services.Email{
		To:      to,
		Subject: "All your Lumaely devices were signed out",
		Body:    fmt.Sprintf("Every device signed in to your Lumaely account was signed out.\nTime: %s\n\n%s", utc(at), ifNotYou),
	}
}

func noticeDiaryReset(to string, at time.Time) services.Email {
	return services.Email{
		To:      to,
		Subject: "Your Lumaely diary was erased",
		Body:    fmt.Sprintf("Every entry and file in your Lumaely diary was erased. Your account is kept.\nTime: %s\n\n%s", utc(at), ifNotYou),
	}
}
