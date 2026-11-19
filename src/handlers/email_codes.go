package handlers

import (
	"cloud/src/services"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	codeSignup     = "signup"
	codeErase      = "erase"
	codeResendWait = 60 * time.Second
	codeDailyCap   = 10
	codeCapWindow  = 24 * time.Hour
)

var errCodeInvalid = errors.New("invalid or expired code")

// issueCode emails a fresh code for (email, kind) unless one was sent within codeResendWait
// or codeDailyCap codes were sent in the current codeCapWindow.
func (h *Handler) issueCode(ctx context.Context, email, kind string, emailKind services.EmailKind) error {
	code, err := h.claimCode(ctx, email, kind)
	if err != nil || code == "" {
		return err
	}
	return h.sendCode(ctx, email, kind, code, emailKind)
}

// claimCode stores a fresh code for (email, kind) and returns it, or "" while the throttle or daily cap holds.
func (h *Handler) claimCode(ctx context.Context, email, kind string) (string, error) {
	code, err := randomDigits(6)
	if err != nil {
		return "", err
	}
	// sent_at claims the slot in the same statement as the throttle check, so concurrent requests send once.
	err = h.DB.QueryRowContext(ctx, `
		INSERT INTO email_codes (email, kind, secret_hash, expires_at, sent_at, window_start, window_count)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), 1)
		ON CONFLICT (email, kind) DO UPDATE
		SET secret_hash = EXCLUDED.secret_hash, attempts = 0, expires_at = EXCLUDED.expires_at, sent_at = NOW(),
			window_start = CASE WHEN email_codes.window_start < NOW() - make_interval(secs => $7) THEN NOW() ELSE email_codes.window_start END,
			window_count = CASE WHEN email_codes.window_start < NOW() - make_interval(secs => $7) THEN 1 ELSE email_codes.window_count + 1 END
		WHERE (email_codes.sent_at IS NULL OR email_codes.sent_at < NOW() - make_interval(secs => $5))
			AND (email_codes.window_count < $6 OR email_codes.window_start < NOW() - make_interval(secs => $7))
		RETURNING 1
	`, email, kind, h.codeHash(email, kind, code), time.Now().Add(challengeTTL), codeResendWait.Seconds(), codeDailyCap, codeCapWindow.Seconds()).Scan(new(int))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return code, nil
}

// sendCode emails a claimed code; on failure it releases the slot so an immediate retry sends.
func (h *Handler) sendCode(ctx context.Context, email, kind, code string, emailKind services.EmailKind) error {
	e, err := h.Emails.Render(emailKind, email, services.EmailData{Code: code})
	if err == nil {
		err = h.Mailer.Send(ctx, e)
	}
	if err == nil {
		return nil
	}
	if _, dbErr := h.DB.ExecContext(context.WithoutCancel(ctx), `
		UPDATE email_codes SET sent_at = NULL, window_count = window_count - 1
		WHERE email = $1 AND kind = $2 AND secret_hash = $3
	`, email, kind, h.codeHash(email, kind, code)); dbErr != nil {
		log.Printf("%s code slot for %q not released: %v", kind, email, dbErr)
	}
	return err
}

// checkCode consumes a matching code; a wrong guess counts toward maxCodeAttempts, which burns it.
// A burned row keeps sent_at and the window, so the throttle and cap still hold.
func (h *Handler) checkCode(ctx context.Context, q queryer, email, kind, code string) error {
	res, err := q.ExecContext(ctx, `
		DELETE FROM email_codes
		WHERE email = $1 AND kind = $2 AND secret_hash = $3 AND expires_at > NOW() AND attempts < $4
	`, email, kind, h.codeHash(email, kind, code), maxCodeAttempts)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}

	var attempts int
	err = q.QueryRowContext(ctx, `
		UPDATE email_codes SET attempts = attempts + 1
		WHERE email = $1 AND kind = $2 AND secret_hash IS NOT NULL AND expires_at > NOW()
		RETURNING attempts
	`, email, kind).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return errCodeInvalid
	}
	if err != nil {
		return err
	}
	if attempts >= maxCodeAttempts {
		if _, err := q.ExecContext(ctx, `UPDATE email_codes SET secret_hash = NULL WHERE email = $1 AND kind = $2`, email, kind); err != nil {
			return err
		}
	}
	return errCodeInvalid
}

// codeHash keys the hash with the server secret: 6 digits are too few to resist brute force on a leaked table.
func (h *Handler) codeHash(email, kind, code string) string {
	mac := hmac.New(sha256.New, h.Settings.ServerSecret)
	mac.Write([]byte("email-code:" + kind + ":" + email + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// notify sends a notice in the background; a failed notice never fails the request.
func (h *Handler) notify(kind services.EmailKind, to string, data services.EmailData) {
	h.inBackground(func() {
		e, err := h.Emails.Render(kind, to, data)
		if err == nil {
			err = h.Mailer.Send(context.Background(), e)
		}
		if err != nil {
			log.Printf("notice %s to %q failed: %v", kind, to, err)
		}
	})
}

// inBackground runs fn after the response; WaitNotices waits for it.
func (h *Handler) inBackground(fn func()) {
	h.notices.Add(1)
	go func() {
		defer h.notices.Done()
		fn()
	}()
}

// WaitNotices blocks until every pending background email is sent or failed.
func (h *Handler) WaitNotices() {
	h.notices.Wait()
}

func sendCodeFailed(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to send email"})
}

// PruneEmailCodes deletes rows whose code expired and whose throttle and cap windows ended.
// A live window must survive: deleting it would reset the throttle or the cap.
func PruneEmailCodes(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM email_codes
		WHERE expires_at <= NOW()
			AND (sent_at IS NULL OR sent_at < NOW() - make_interval(secs => $1))
			AND window_start < NOW() - make_interval(secs => $2)
	`, codeResendWait.Seconds(), codeCapWindow.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneEmailCodesEvery prunes on each tick until ctx ends.
func PruneEmailCodesEvery(ctx context.Context, db *sql.DB, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := PruneEmailCodes(ctx, db); err != nil {
				log.Printf("email codes prune failed: %v", err)
			} else if n > 0 {
				log.Printf("pruned %d stale email codes", n)
			}
		}
	}
}
