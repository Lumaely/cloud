package handlers

import (
	"cloud/models"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
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

	c.JSON(http.StatusOK, entries)
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

	if isDeleted == "true" {
		if entryID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "entry_id is required for deletion"})
			return
		}
		query := `
			UPDATE mood_entries 
			SET deleted_at = NOW(), updated_at = NOW() 
			WHERE entry_id = $1 AND user_id = $2
		`
		_, err := h.DB.ExecContext(c.Request.Context(), query, entryID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record tombstone"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted"})
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

	var existingID string
	checkQuery := `
		SELECT entry_id FROM mood_entries 
		WHERE user_id = $1 AND sha256_hash = $2 AND deleted_at IS NULL
	`
	err = h.DB.QueryRowContext(c.Request.Context(), checkQuery, userID, sha256Hash).Scan(&existingID)
	if err == nil && existingID != entryID {
		c.JSON(http.StatusConflict, gin.H{"error": "Entry conflict or duplicate hash detected"})
		return
	}

	tx, err := h.DB.BeginTx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction initiation failed"})
		return
	}
	defer tx.Rollback()

	var targetEntryID string
	if entryID != "" {
		targetEntryID = entryID
		updateQuery := `
			UPDATE mood_entries
			SET mood_scale = $1, metadata_blob = $2, entry_timestamp = $3, sha256_hash = $4, updated_at = NOW()
			WHERE entry_id = $5 AND user_id = $6
		`
		_, err = tx.ExecContext(c.Request.Context(), updateQuery, moodScale, metadataBlob, entryTime, sha256Hash, targetEntryID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update entry"})
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

	files := form.File["file_content"]
	aesKeys := form.Value["encrypted_aes_key"]
	mediaTypes := form.Value["media_type"]

	if len(files) > 0 {
		for i, fileHeader := range files {
			var aesKey []byte
			if i < len(aesKeys) {
				aesKey = []byte(aesKeys[i])
			}
			mediaType := "application/octet-stream"
			if i < len(mediaTypes) {
				mediaType = mediaTypes[i]
			}

			randomBytes := make([]byte, 16)
			rand.Read(randomBytes)
			objectKey := userID + "/" + targetEntryID + "/" + hex.EncodeToString(randomBytes)

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

	c.JSON(http.StatusCreated, gin.H{
		"status":   "synced",
		"entry_id": targetEntryID,
	})
}
