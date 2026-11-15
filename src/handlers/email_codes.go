package handlers

import (
	"cloud/src/services"
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	codeSignup     = "signup"
	codeErase      = "erase"
	codeDelete     = "delete"
	codeResendWait = 60 * time.Second
)

var errCodeInvalid = errors.New("invalid or expired code")

// issueCode emails a fresh code for (email, kind) unless one was sent within codeResendWait.
func (h *Handler) issueCode(ctx context.Context, email, kind string, compose func(code string) services.Email) error {
	code, err := randomDigits(6)
	if err != nil {
		return err
	}
	secretHash := hashSecret(code)
	// sent_at claims the slot in the same statement as the throttle check, so concurrent requests send once.
	err = h.DB.QueryRowContext(ctx, `
		INSERT INTO email_codes (email, kind, secret_hash, expires_at, sent_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (email, kind) DO UPDATE
		SET secret_hash = EXCLUDED.secret_hash, attempts = 0, expires_at = EXCLUDED.expires_at, sent_at = NOW()
		WHERE email_codes.sent_at IS NULL OR email_codes.sent_at < NOW() - make_interval(secs => $5)
		RETURNING 1
	`, email, kind, secretHash, time.Now().Add(challengeTTL), codeResendWait.Seconds()).Scan(new(int))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := h.Mailer.Send(ctx, compose(code)); err != nil {
		// Release the slot so an immediate retry sends.
		if _, dbErr := h.DB.ExecContext(context.WithoutCancel(ctx), `
			UPDATE email_codes SET sent_at = NULL WHERE email = $1 AND kind = $2 AND secret_hash = $3
		`, email, kind, secretHash); dbErr != nil {
			log.Printf("%s code slot for %q not released: %v", kind, email, dbErr)
		}
		return err
	}
	return nil
}

// checkCode consumes a matching code; a wrong guess counts toward maxCodeAttempts, which burns it.
func (h *Handler) checkCode(ctx context.Context, q queryer, email, kind, code string) error {
	res, err := q.ExecContext(ctx, `
		DELETE FROM email_codes
		WHERE email = $1 AND kind = $2 AND secret_hash = $3 AND expires_at > NOW() AND attempts < $4
	`, email, kind, hashSecret(code), maxCodeAttempts)
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
		WHERE email = $1 AND kind = $2 AND expires_at > NOW()
		RETURNING attempts
	`, email, kind).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return errCodeInvalid
	}
	if err != nil {
		return err
	}
	if attempts >= maxCodeAttempts {
		if _, err := q.ExecContext(ctx, `DELETE FROM email_codes WHERE email = $1 AND kind = $2`, email, kind); err != nil {
			return err
		}
	}
	return errCodeInvalid
}

// notify sends e in the background; a failed notice never fails the request.
func (h *Handler) notify(e services.Email) {
	h.notices.Add(1)
	go func() {
		defer h.notices.Done()
		if err := h.Mailer.Send(context.Background(), e); err != nil {
			log.Printf("notice %q to %q failed: %v", e.Subject, e.To, err)
		}
	}()
}

// WaitNotices blocks until every pending notice is sent or failed.
func (h *Handler) WaitNotices() {
	h.notices.Wait()
}

func sendCodeFailed(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to send email"})
}
