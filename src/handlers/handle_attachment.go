package handlers

import (
	"cloud/src/models"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	maxPreviewBytes = 256 * 1024
	octetStream     = "application/octet-stream"
)

var errQuotaExceeded = errors.New("storage quota exceeded")

// attachmentID accepts only the lowercase UUIDv7 form the app stores in its entry blobs.
func attachmentID(c *gin.Context) (string, bool) {
	id := c.Param("attachment_id")
	if len(id) != 36 || !validRecordID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "attachment_id must be a lowercase UUIDv7"})
		return "", false
	}
	return id, true
}

// uploadLength reads the declared body size; uploads stream to storage, so it must be known.
func uploadLength(c *gin.Context) (int64, bool) {
	switch n := c.Request.ContentLength; {
	case n < 0:
		c.JSON(http.StatusLengthRequired, gin.H{"error": "Content-Length is required"})
		return 0, false
	case n == 0:
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is empty"})
		return 0, false
	default:
		return n, true
	}
}

type storedAttachment struct {
	key        string
	previewKey sql.NullString
	size       int64
}

func lockAttachment(ctx context.Context, tx *sql.Tx, userID, id string) (storedAttachment, error) {
	var a storedAttachment
	err := tx.QueryRowContext(ctx, `
		SELECT s3_object_key, preview_object_key, size_bytes FROM attachments
		WHERE user_id = $1 AND attachment_id = $2 FOR UPDATE
	`, userID, id).Scan(&a.key, &a.previewKey, &a.size)
	return a, err
}

func loadAttachment(ctx context.Context, q queryer, userID, id string) (models.Attachment, error) {
	a := models.Attachment{AttachmentID: id}
	err := q.QueryRowContext(ctx, `
		SELECT size_bytes, preview_size_bytes, created_at FROM attachments WHERE user_id = $1 AND attachment_id = $2
	`, userID, id).Scan(&a.SizeBytes, &a.PreviewSizeBytes, &a.CreatedAt)
	return a, err
}

// lockUser serializes a user's uploads so concurrent ones can't both pass the quota check.
func lockUser(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE user_id = $1 FOR UPDATE`, userID)
	return err
}

func (h *Handler) checkQuota(ctx context.Context, q queryer, userID string, incoming int64) error {
	if h.Settings.StorageQuota <= 0 {
		return nil
	}
	used, err := storageUsed(ctx, q, userID)
	if err != nil {
		return err
	}
	if used+incoming > h.Settings.StorageQuota {
		return errQuotaExceeded
	}
	return nil
}

// UploadAttachment stores a new immutable file. The upload runs before the transaction so a long
// video never holds the user's lock; each upload gets its own key, so a losing retry deletes only its copy.
func (h *Handler) UploadAttachment(c *gin.Context) {
	id, ok := attachmentID(c)
	if !ok {
		return
	}
	size, ok := uploadLength(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	if _, err := loadAttachment(ctx, h.DB, userID, id); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Attachment already exists"})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		serverError(c, "Database error")
		return
	}
	if err := h.checkQuota(ctx, h.DB, userID, size); err != nil {
		h.respondUploadError(c, err)
		return
	}

	key := newObjectKey(userID, id)
	if err := h.Storage.Upload(ctx, key, c.Request.Body, size, octetStream); err != nil {
		h.deleteObjects([]string{key})
		serverError(c, "Storage upload error")
		return
	}

	err := h.inTx(ctx, func(tx *sql.Tx) error {
		if err := lockUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := h.checkQuota(ctx, tx, userID, size); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO attachments (user_id, attachment_id, s3_object_key, size_bytes) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING
		`, userID, id, key, size)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errAttachmentExists
		}
		return nil
	})
	if err != nil {
		h.deleteObjects([]string{key})
		h.respondUploadError(c, err)
		return
	}
	h.respondAttachment(c, userID, id)
}

var (
	errAttachmentExists  = errors.New("attachment already exists")
	errPreviewExists     = errors.New("preview already exists")
	errAttachmentMissing = errors.New("attachment not found")
	errPreviewTooLarge   = errors.New("preview is larger than its file")
)

// UploadPreview adds the preview of an existing file. Previews are outside the quota: each is capped at
// maxPreviewBytes and, when there is a quota, at its file's size, so storage stays under twice the quota.
func (h *Handler) UploadPreview(c *gin.Context) {
	id, ok := attachmentID(c)
	if !ok {
		return
	}
	size, ok := uploadLength(c)
	if !ok {
		return
	}
	if size > maxPreviewBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("a preview is at most %d bytes", maxPreviewBytes)})
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	precheck := func(q *sql.Tx) error {
		a, err := lockAttachment(ctx, q, userID, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return errAttachmentMissing
		case err != nil:
			return err
		case a.previewKey.Valid:
			return errPreviewExists
		case h.Settings.StorageQuota > 0 && size > a.size:
			return errPreviewTooLarge
		}
		return nil
	}
	if err := h.inTx(ctx, precheck); err != nil {
		h.respondUploadError(c, err)
		return
	}

	key := newObjectKey(userID, id)
	if err := h.Storage.Upload(ctx, key, c.Request.Body, size, octetStream); err != nil {
		h.deleteObjects([]string{key})
		serverError(c, "Storage upload error")
		return
	}
	err := h.inTx(ctx, func(tx *sql.Tx) error {
		if err := precheck(tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE attachments SET preview_object_key = $3, preview_size_bytes = $4 WHERE user_id = $1 AND attachment_id = $2
		`, userID, id, key, size)
		return err
	})
	if err != nil {
		h.deleteObjects([]string{key})
		h.respondUploadError(c, err)
		return
	}
	h.respondAttachment(c, userID, id)
}

func (h *Handler) respondUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errQuotaExceeded):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	case errors.Is(err, errAttachmentExists), errors.Is(err, errPreviewExists):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, errAttachmentMissing):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, errPreviewTooLarge):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		log.Printf("attachment upload failed: %v", err)
		serverError(c, "Database error")
	}
}

func (h *Handler) respondAttachment(c *gin.Context, userID, id string) {
	a, err := loadAttachment(c.Request.Context(), h.DB, userID, id)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusCreated, a)
}

func (h *Handler) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *Handler) ListAttachments(c *gin.Context) {
	rows, err := h.DB.QueryContext(c.Request.Context(), `
		SELECT attachment_id, size_bytes, preview_size_bytes, created_at FROM attachments
		WHERE user_id = $1 ORDER BY created_at ASC, attachment_id ASC
	`, c.GetString("userID"))
	if err != nil {
		serverError(c, "Database error")
		return
	}
	defer rows.Close()
	list := make([]models.Attachment, 0)
	for rows.Next() {
		var a models.Attachment
		if err := rows.Scan(&a.AttachmentID, &a.SizeBytes, &a.PreviewSizeBytes, &a.CreatedAt); err != nil {
			serverError(c, "Database error")
			return
		}
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, list)
}

func (h *Handler) DownloadAttachment(c *gin.Context) {
	h.serveAttachment(c, "s3_object_key")
}

func (h *Handler) DownloadAttachmentPreview(c *gin.Context) {
	h.serveAttachment(c, "preview_object_key")
}

// serveAttachment streams keyColumn of the caller's attachment; Range requests get 206.
func (h *Handler) serveAttachment(c *gin.Context, keyColumn string) {
	id, ok := attachmentID(c)
	if !ok {
		return
	}
	var objectKey sql.NullString
	err := h.DB.QueryRowContext(c.Request.Context(), `
		SELECT `+keyColumn+` FROM attachments WHERE user_id = $1 AND attachment_id = $2
	`, c.GetString("userID"), id).Scan(&objectKey)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !objectKey.Valid) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}

	obj, err := h.Storage.Download(c.Request.Context(), objectKey.String)
	if err != nil {
		serverError(c, "Storage download error")
		return
	}
	defer obj.Close()

	c.Header("Content-Type", octetStream)
	http.ServeContent(c.Writer, c.Request, "", obj.LastModified, obj)
}

// DeleteAttachment removes a file and its preview; objects go first so a storage failure keeps the row for a retry.
func (h *Handler) DeleteAttachment(c *gin.Context) {
	id, ok := attachmentID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	err := h.inTx(ctx, func(tx *sql.Tx) error {
		a, err := lockAttachment(ctx, tx, userID, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errAttachmentMissing
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE user_id = $1 AND attachment_id = $2`, userID, id); err != nil {
			return err
		}
		keys := []string{a.key}
		if a.previewKey.Valid {
			keys = append(keys, a.previewKey.String)
		}
		for _, k := range keys {
			if err := h.Storage.Delete(ctx, k); err != nil {
				return fmt.Errorf("storage delete: %w", err)
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errAttachmentMissing):
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
	case err != nil:
		log.Printf("attachment delete failed: %v", err)
		serverError(c, "Attachment delete failed")
	default:
		c.Status(http.StatusNoContent)
	}
}

func newObjectKey(userID, attachmentID string) string {
	randomBytes := make([]byte, 16)
	rand.Read(randomBytes)
	return userID + "/" + attachmentID + "/" + hex.EncodeToString(randomBytes)
}

// Detached ctx: cleanup must outlive a cancelled request.
func (h *Handler) deleteObjects(keys []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, key := range keys {
		if err := h.Storage.Delete(ctx, key); err != nil {
			log.Printf("orphan object %q not deleted: %v", key, err)
		}
	}
}
