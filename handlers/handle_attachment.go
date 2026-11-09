package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *Handler) DownloadAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	attachmentID := c.Param("attachment_id")
	if err := uuid.Validate(attachmentID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "attachment_id must be a UUID"})
		return
	}

	var objectKey, mediaType string
	query := `
		SELECT a.s3_object_key, a.media_type
		FROM entry_attachments a
		JOIN mood_entries e ON e.entry_id = a.entry_id
		WHERE a.attachment_id = $1 AND e.user_id = $2 AND e.deleted_at IS NULL
	`
	err := h.DB.QueryRowContext(c.Request.Context(), query, attachmentID, userID).Scan(&objectKey, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	body, size, err := h.Storage.Download(c.Request.Context(), objectKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Storage download error"})
		return
	}
	defer body.Close()

	c.DataFromReader(http.StatusOK, size, mediaType, body, nil)
}
