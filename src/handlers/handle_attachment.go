package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) DownloadAttachment(c *gin.Context) {
	h.serveAttachment(c, "a.s3_object_key")
}

func (h *Handler) DownloadAttachmentPreview(c *gin.Context) {
	h.serveAttachment(c, "a.preview_object_key")
}

// serveAttachment streams keyColumn of a live entry owned by the caller; Range requests get 206.
func (h *Handler) serveAttachment(c *gin.Context, keyColumn string) {
	attachmentID, ok := canonicalUUID(c.Param("attachment_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "attachment_id must be a UUID"})
		return
	}

	var objectKey sql.NullString
	var mediaType string
	err := h.DB.QueryRowContext(c.Request.Context(), `
		SELECT `+keyColumn+`, a.media_type
		FROM entry_attachments a
		JOIN mood_entries e ON e.entry_id = a.entry_id
		WHERE a.attachment_id = $1 AND e.user_id = $2 AND e.deleted_at IS NULL
	`, attachmentID, c.GetString("userID")).Scan(&objectKey, &mediaType)
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

	c.Header("Content-Type", mediaType)
	http.ServeContent(c.Writer, c.Request, "", obj.LastModified, obj)
}

// DeleteAttachment removes one file; the entry gets a new version so other devices refetch it.
func (h *Handler) DeleteAttachment(c *gin.Context) {
	attachmentID, ok := canonicalUUID(c.Param("attachment_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "attachment_id must be a UUID"})
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	seq, err := nextSeq(ctx, tx, userID)
	if err != nil {
		serverError(c, "Database error")
		return
	}

	var entryID, key string
	var previewKey sql.NullString
	err = tx.QueryRowContext(ctx, `
		DELETE FROM entry_attachments a
		USING mood_entries e
		WHERE a.attachment_id = $1 AND e.entry_id = a.entry_id AND e.user_id = $2 AND e.deleted_at IS NULL
		RETURNING a.entry_id, a.s3_object_key, a.preview_object_key
	`, attachmentID, userID).Scan(&entryID, &key, &previewKey)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE mood_entries SET version = version + 1, seq = $1, updated_at = NOW() WHERE entry_id = $2
	`, seq, entryID)
	if err != nil {
		serverError(c, "Database error")
		return
	}

	// Objects deleted before commit: a storage failure rolls back so the client can retry.
	keys := []string{key}
	if previewKey.Valid {
		keys = append(keys, previewKey.String)
	}
	for _, k := range keys {
		if err := h.Storage.Delete(ctx, k); err != nil {
			serverError(c, "Storage delete error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(c, "Transaction commit failed")
		return
	}
	c.Status(http.StatusNoContent)
}
