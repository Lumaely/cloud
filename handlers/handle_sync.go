package handlers

import (
	"cloud/models"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *Handler) FetchDelta(c *gin.Context) {
	userID := c.GetString("userID")
	sinceStr := c.Query("since")
	if sinceStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Parameter 'since' is required"})
		return
	}

	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid RFC3339 timestamp"})
		return
	}

	query := `
		SELECT entry_id, mood_scale, metadata_blob, entry_timestamp, sha256_hash, updated_at, deleted_at
		FROM mood_entries
		WHERE user_id = $1 AND (updated_at > $2 OR (deleted_at IS NOT NULL AND deleted_at > $2))
		ORDER BY updated_at ASC
	`
	rows, err := h.DB.QueryContext(c.Request.Context(), query, userID, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database query failed"})
		return
	}
	defer rows.Close()

	entries := make([]models.MoodEntry, 0)
	for rows.Next() {
		var entry models.MoodEntry
		err := rows.Scan(
			&entry.EntryID,
			&entry.MoodScale,
			&entry.MetadataBlob,
			&entry.EntryTimestamp,
			&entry.SHA256Hash,
			&entry.UpdatedAt,
			&entry.DeletedAt,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse records"})
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse records"})
		return
	}

	attachments, err := h.deltaAttachments(c.Request.Context(), userID, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load attachments"})
		return
	}
	for i := range entries {
		entries[i].Attachments = attachments[entries[i].EntryID]
		if entries[i].Attachments == nil {
			entries[i].Attachments = []models.Attachment{}
		}
	}

	c.JSON(http.StatusOK, entries)
}

func (h *Handler) deltaAttachments(ctx context.Context, userID string, since time.Time) (map[string][]models.Attachment, error) {
	query := `
		SELECT a.entry_id, a.attachment_id, a.encrypted_aes_key, a.media_type
		FROM entry_attachments a
		JOIN mood_entries e ON e.entry_id = a.entry_id
		WHERE e.user_id = $1 AND (e.updated_at > $2 OR (e.deleted_at IS NOT NULL AND e.deleted_at > $2))
		ORDER BY a.created_at ASC
	`
	rows, err := h.DB.QueryContext(ctx, query, userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byEntry := map[string][]models.Attachment{}
	for rows.Next() {
		var entryID string
		var a models.Attachment
		if err := rows.Scan(&entryID, &a.AttachmentID, &a.EncryptedAESKey, &a.MediaType); err != nil {
			return nil, err
		}
		byEntry[entryID] = append(byEntry[entryID], a)
	}
	return byEntry, rows.Err()
}

func (h *Handler) PushChanges(c *gin.Context) {
	userID := c.GetString("userID")

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse multipart form"})
		return
	}

	entryID := c.PostForm("entry_id")
	isDeleted := c.PostForm("is_deleted")
	if entryID != "" {
		if err := uuid.Validate(entryID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "entry_id must be a UUID"})
			return
		}
	}

	if isDeleted == "true" {
		if entryID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "entry_id is required for deletion"})
			return
		}
		h.tombstoneEntry(c, userID, entryID)
		return
	}

	moodScale := []byte(c.PostForm("mood_scale"))
	metadataBlob := []byte(c.PostForm("metadata_blob"))
	sha256Hash := c.PostForm("sha256_hash")
	entryTimeStr := c.PostForm("entry_timestamp")

	entryTime, err := time.Parse(time.RFC3339, entryTimeStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid entry_timestamp format"})
		return
	}

	files := form.File["file_content"]
	aesKeys := form.Value["encrypted_aes_key"]
	mediaTypes := form.Value["media_type"]
	if len(aesKeys) < len(files) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Each file_content requires an encrypted_aes_key"})
		return
	}

	var existingID string
	checkQuery := `
		SELECT entry_id FROM mood_entries 
		WHERE user_id = $1 AND sha256_hash = $2 AND deleted_at IS NULL
	`
	err = h.DB.QueryRowContext(c.Request.Context(), checkQuery, userID, sha256Hash).Scan(&existingID)
	switch {
	case err == nil && existingID != entryID:
		c.JSON(http.StatusConflict, gin.H{"error": "Entry conflict or duplicate hash detected"})
		return
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Duplicate check failed"})
		return
	}

	tx, err := h.DB.BeginTx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction initiation failed"})
		return
	}
	defer tx.Rollback()

	var uploadedKeys []string
	committed := false
	defer func() {
		if !committed {
			h.deleteObjects(uploadedKeys)
		}
	}()

	var targetEntryID string
	if entryID != "" {
		targetEntryID = entryID
		updateQuery := `
			UPDATE mood_entries
			SET mood_scale = $1, metadata_blob = $2, entry_timestamp = $3, sha256_hash = $4, updated_at = NOW()
			WHERE entry_id = $5 AND user_id = $6
		`
		res, err := tx.ExecContext(c.Request.Context(), updateQuery, moodScale, metadataBlob, entryTime, sha256Hash, targetEntryID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update entry"})
			return
		}
		// 0 rows: not the caller's entry.
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "Entry not found"})
			return
		}
	} else {
		insertQuery := `
			INSERT INTO mood_entries (user_id, mood_scale, metadata_blob, entry_timestamp, sha256_hash, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			RETURNING entry_id
		`
		err = tx.QueryRowContext(c.Request.Context(), insertQuery, userID, moodScale, metadataBlob, entryTime, sha256Hash).Scan(&targetEntryID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create entry"})
			return
		}
	}

	if len(files) > 0 {
		for i, fileHeader := range files {
			aesKey := []byte(aesKeys[i])
			mediaType := "application/octet-stream"
			if i < len(mediaTypes) {
				mediaType = mediaTypes[i]
			}

			objectKey := newObjectKey(userID, targetEntryID)

			src, err := fileHeader.Open()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read attachment stream"})
				return
			}

			err = h.Storage.Upload(c.Request.Context(), objectKey, src, fileHeader.Size, mediaType)
			src.Close()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Storage upload error"})
				return
			}
			uploadedKeys = append(uploadedKeys, objectKey)

			attQuery := `
				INSERT INTO entry_attachments (entry_id, s3_object_key, encrypted_aes_key, media_type)
				VALUES ($1, $2, $3, $4)
			`
			_, err = tx.ExecContext(c.Request.Context(), attQuery, targetEntryID, objectKey, aesKey, mediaType)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to catalog attachment"})
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction commit failed"})
		return
	}
	committed = true

	c.JSON(http.StatusCreated, gin.H{
		"status":   "synced",
		"entry_id": targetEntryID,
	})
}

func newObjectKey(userID, entryID string) string {
	randomBytes := make([]byte, 16)
	rand.Read(randomBytes)
	return userID + "/" + entryID + "/" + hex.EncodeToString(randomBytes)
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

// Objects deleted before commit: a storage failure rolls back so the client can retry.
func (h *Handler) tombstoneEntry(c *gin.Context, userID, entryID string) {
	ctx := c.Request.Context()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction initiation failed"})
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE mood_entries
		SET deleted_at = NOW(), updated_at = NOW()
		WHERE entry_id = $1 AND user_id = $2
	`, entryID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record tombstone"})
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Entry not found"})
		return
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_attachments WHERE entry_id = $1`, entryID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove attachments"})
		return
	}
	if err := h.Storage.DeletePrefix(ctx, userID+"/"+entryID+"/"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Storage delete error"})
		return
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction commit failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
