package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cloud/src/models"

	"github.com/gin-gonic/gin"
)

const (
	defaultPageSize = 200
	maxPageSize     = 500
	keyLength       = 32
	kdfSaltLength   = 16
	maxWrappedKey   = 1024
	maxBlobBytes    = 256 * 1024
)

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// nextSeq allocates the user's next change number; its row lock serializes the user's writes,
// so it must be the first statement of every write transaction.
func nextSeq(ctx context.Context, tx *sql.Tx, userID string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `UPDATE users SET change_seq = change_seq + 1 WHERE user_id = $1 RETURNING change_seq`, userID).Scan(&seq)
	return seq, err
}

type pageRequest struct {
	cursor int64
	limit  int
}

func parsePage(c *gin.Context) (pageRequest, error) {
	p := pageRequest{limit: defaultPageSize}
	if v := c.Query("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return p, errors.New("cursor must be a non-negative integer")
		}
		p.cursor = n
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return p, errors.New("limit must be between 1 and 500")
		}
		p.limit = n
	}
	return p, nil
}

// pageMeta fills everything but Items; items holds up to limit+1 rows to detect more pages.
func pageMeta[T any](ctx context.Context, q queryer, table, userID string, p pageRequest, items []T, seqOf func(T) int64) (models.Page[T], error) {
	page := models.Page[T]{Items: items, NextCursor: p.cursor}
	if len(items) > p.limit {
		page.Items = items[:p.limit]
		page.HasMore = true
	}
	if n := len(page.Items); n > 0 {
		page.NextCursor = seqOf(page.Items[n-1])
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE user_id = $1 AND seq > $2`, userID, p.cursor).Scan(&page.Total); err != nil {
		return page, err
	}
	err := q.QueryRowContext(ctx, `SELECT diary_epoch FROM users WHERE user_id = $1`, userID).Scan(&page.DiaryEpoch)
	return page, err
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateKey(name string, key []byte) error {
	if len(key) != keyLength {
		return errors.New(name + " must be 32 bytes")
	}
	return nil
}

func validateKeyMaterial(k models.KeyMaterial) error {
	if len(k.KDFSalt) != kdfSaltLength {
		return errors.New("kdf_salt must be 16 bytes")
	}
	var params map[string]any
	if err := json.Unmarshal(k.KDFParams, &params); err != nil {
		return errors.New("kdf_params must be a JSON object")
	}
	return validateWrapped("wrapped_key_password", k.WrappedKeyPassword)
}

func validateRecoveryMaterial(r models.RecoveryMaterial) error {
	if err := validateKey("recovery_auth_key", r.RecoveryAuthKey); err != nil {
		return err
	}
	if err := validateWrapped("wrapped_key_recovery", r.WrappedKeyRecovery); err != nil {
		return err
	}
	return validateWrapped("encrypted_recovery_key", r.EncryptedRecoveryKey)
}

func validateWrapped(name string, b []byte) error {
	if len(b) == 0 || len(b) > maxWrappedKey {
		return errors.New(name + " must be 1 to 1024 bytes")
	}
	return nil
}

func badRequest(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func serverError(c *gin.Context, msg string) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
}
