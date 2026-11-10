package handlers

import (
	"cloud/src/models"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	maxOperations         = 100
	maxAttachmentsPerItem = 10
	maxPreviewBytes       = 256 * 1024
	entryColumns          = `entry_id, mood_scale, metadata_blob, entry_timestamp, sha256_hash, version, seq, updated_at, deleted_at`

	statusCreated       = "created"
	statusUpdated       = "updated"
	statusDeleted       = "deleted"
	statusConflict      = "conflict"
	statusNotFound      = "not_found"
	statusInvalid       = "invalid"
	statusQuotaExceeded = "quota_exceeded"
	statusError         = "error"
)

func scanEntry(row interface{ Scan(...any) error }) (models.MoodEntry, error) {
	var e models.MoodEntry
	err := row.Scan(&e.EntryID, &e.MoodScale, &e.MetadataBlob, &e.EntryTimestamp, &e.SHA256Hash, &e.Version, &e.Seq, &e.UpdatedAt, &e.DeletedAt)
	e.Attachments = []models.Attachment{}
	return e, err
}

// attachEntries fills Attachments of entries in place.
func attachEntries(ctx context.Context, q queryer, entries []models.MoodEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]string, len(entries))
	index := make(map[string]int, len(entries))
	for i, e := range entries {
		ids[i] = e.EntryID
		index[e.EntryID] = i
	}
	rows, err := q.QueryContext(ctx, `
		SELECT entry_id, attachment_id, encrypted_aes_key, media_type, size_bytes, preview_size_bytes, preview_object_key IS NOT NULL
		FROM entry_attachments
		WHERE entry_id = ANY($1::uuid[])
		ORDER BY created_at ASC
	`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var entryID string
		var a models.Attachment
		if err := rows.Scan(&entryID, &a.AttachmentID, &a.EncryptedAESKey, &a.MediaType, &a.SizeBytes, &a.PreviewSizeBytes, &a.HasPreview); err != nil {
			return err
		}
		i := index[entryID]
		entries[i].Attachments = append(entries[i].Attachments, a)
	}
	return rows.Err()
}

func loadEntry(ctx context.Context, q queryer, userID, entryID string) (models.MoodEntry, error) {
	e, err := scanEntry(q.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM mood_entries WHERE entry_id = $1 AND user_id = $2`, entryID, userID))
	if err != nil {
		return e, err
	}
	list := []models.MoodEntry{e}
	err = attachEntries(ctx, q, list)
	return list[0], err
}

func (h *Handler) FetchDelta(c *gin.Context) {
	p, err := parsePage(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	rows, err := h.DB.QueryContext(ctx, `
		SELECT `+entryColumns+` FROM mood_entries
		WHERE user_id = $1 AND seq > $2
		ORDER BY seq ASC
		LIMIT $3
	`, userID, p.cursor, p.limit+1)
	if err != nil {
		serverError(c, "Database query failed")
		return
	}
	defer rows.Close()

	entries := make([]models.MoodEntry, 0)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			serverError(c, "Failed to parse records")
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		serverError(c, "Failed to parse records")
		return
	}

	page, err := pageMeta(ctx, h.DB, "mood_entries", userID, p, entries, func(e models.MoodEntry) int64 { return e.Seq })
	if err != nil {
		serverError(c, "Database query failed")
		return
	}
	if err := attachEntries(ctx, h.DB, page.Items); err != nil {
		serverError(c, "Failed to load attachments")
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) PushChanges(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		badRequest(c, errors.New("failed to parse multipart form"))
		return
	}
	raw := form.Value["operations"]
	if len(raw) != 1 {
		badRequest(c, errors.New("one operations part is required"))
		return
	}
	var ops []models.EntryOperation
	if err := json.Unmarshal([]byte(raw[0]), &ops); err != nil {
		badRequest(c, errors.New("operations must be a JSON array"))
		return
	}
	if len(ops) == 0 || len(ops) > maxOperations {
		badRequest(c, fmt.Errorf("operations must hold 1 to %d items", maxOperations))
		return
	}

	results := make([]models.OperationResult, len(ops))
	for i, op := range ops {
		results[i] = h.applyOperation(c.Request.Context(), c.GetString("userID"), op, form.File)
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// opError carries the result status of a failed operation.
type opError struct {
	status string
	msg    string
	entry  *models.MoodEntry
}

func (e *opError) Error() string { return e.msg }

func invalid(msg string) *opError { return &opError{status: statusInvalid, msg: msg} }

type pendingFile struct {
	header    *multipart.FileHeader
	preview   *multipart.FileHeader
	aesKey    []byte
	mediaType string
}

func (p pendingFile) previewSize() int64 {
	if p.preview == nil {
		return 0
	}
	return p.preview.Size
}

func (h *Handler) applyOperation(ctx context.Context, userID string, op models.EntryOperation, files map[string][]*multipart.FileHeader) models.OperationResult {
	res := models.OperationResult{Ref: op.Ref}
	var status string
	var entry *models.MoodEntry
	var err error

	if id, ok := canonicalUUID(op.EntryID); ok {
		op.EntryID = id
	}
	if op.Deleted {
		status, err = h.deleteEntry(ctx, userID, op)
	} else {
		status, entry, err = h.upsertEntry(ctx, userID, op, files)
	}

	var oe *opError
	switch {
	case errors.As(err, &oe):
		res.Status, res.Error, res.Entry = oe.status, oe.msg, oe.entry
	case err != nil:
		log.Printf("sync operation %q for %s failed: %v", op.Ref, userID, err)
		res.Status, res.Error = statusError, "internal error"
	default:
		res.Status, res.Entry = status, entry
	}
	return res
}

func validateOperation(op models.EntryOperation, files map[string][]*multipart.FileHeader) ([]pendingFile, time.Time, error) {
	if op.EntryID != "" {
		if err := uuid.Validate(op.EntryID); err != nil {
			return nil, time.Time{}, invalid("entry_id must be a UUID")
		}
		if op.Version < 1 {
			return nil, time.Time{}, invalid("version is required to update an entry")
		}
	}
	ts, err := time.Parse(time.RFC3339, op.EntryTimestamp)
	if err != nil {
		return nil, ts, invalid("entry_timestamp must be RFC3339")
	}
	if op.SHA256Hash == "" {
		return nil, ts, invalid("sha256_hash is required")
	}
	if len(op.Attachments) > maxAttachmentsPerItem {
		return nil, ts, invalid(fmt.Sprintf("an entry holds at most %d files", maxAttachmentsPerItem))
	}

	pending := make([]pendingFile, 0, len(op.Attachments))
	for _, a := range op.Attachments {
		fh := firstFile(files, a.File)
		if fh == nil {
			return nil, ts, invalid(fmt.Sprintf("file part %q is missing", a.File))
		}
		if len(a.EncryptedAESKey) == 0 {
			return nil, ts, invalid("encrypted_aes_key is required for each file")
		}
		p := pendingFile{header: fh, aesKey: a.EncryptedAESKey, mediaType: a.MediaType}
		if p.mediaType == "" {
			p.mediaType = "application/octet-stream"
		}
		if a.Preview != "" {
			if p.preview = firstFile(files, a.Preview); p.preview == nil {
				return nil, ts, invalid(fmt.Sprintf("preview part %q is missing", a.Preview))
			}
			if p.preview.Size > maxPreviewBytes {
				return nil, ts, invalid(fmt.Sprintf("preview %q exceeds %d bytes", a.Preview, maxPreviewBytes))
			}
		}
		pending = append(pending, p)
	}
	return pending, ts, nil
}

func firstFile(files map[string][]*multipart.FileHeader, name string) *multipart.FileHeader {
	if name == "" || len(files[name]) == 0 {
		return nil
	}
	return files[name][0]
}

func (h *Handler) upsertEntry(ctx context.Context, userID string, op models.EntryOperation, files map[string][]*multipart.FileHeader) (string, *models.MoodEntry, error) {
	pending, ts, err := validateOperation(op, files)
	if err != nil {
		return "", nil, err
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	var uploaded []string
	committed := false
	defer func() {
		if !committed {
			h.deleteObjects(uploaded)
		}
	}()

	seq, err := nextSeq(ctx, tx, userID)
	if err != nil {
		return "", nil, err
	}

	var duplicateID string
	err = tx.QueryRowContext(ctx, `
		SELECT entry_id FROM mood_entries
		WHERE user_id = $1 AND sha256_hash = $2 AND deleted_at IS NULL AND entry_id::text <> $3
	`, userID, op.SHA256Hash, op.EntryID).Scan(&duplicateID)
	switch {
	case err == nil:
		current, err := loadEntry(ctx, tx, userID, duplicateID)
		if err != nil {
			return "", nil, err
		}
		return "", nil, &opError{status: statusConflict, msg: "duplicate sha256_hash", entry: &current}
	case !errors.Is(err, sql.ErrNoRows):
		return "", nil, err
	}

	status := statusCreated
	entryID := op.EntryID
	existing := 0
	if entryID == "" {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO mood_entries (user_id, mood_scale, metadata_blob, entry_timestamp, sha256_hash, seq)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING entry_id
		`, userID, nonNil(op.MoodScale), nonNil(op.MetadataBlob), ts, op.SHA256Hash, seq).Scan(&entryID)
		if err != nil {
			return "", nil, err
		}
	} else {
		status = statusUpdated
		if err := checkVersion(ctx, tx, userID, entryID, op.Version); err != nil {
			return "", nil, err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE mood_entries
			SET mood_scale = $1, metadata_blob = $2, entry_timestamp = $3, sha256_hash = $4,
				version = version + 1, seq = $5, updated_at = NOW()
			WHERE entry_id = $6 AND user_id = $7
		`, nonNil(op.MoodScale), nonNil(op.MetadataBlob), ts, op.SHA256Hash, seq, entryID, userID)
		if err != nil {
			return "", nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entry_attachments WHERE entry_id = $1`, entryID).Scan(&existing); err != nil {
			return "", nil, err
		}
	}

	if existing+len(pending) > maxAttachmentsPerItem {
		return "", nil, invalid(fmt.Sprintf("an entry holds at most %d files", maxAttachmentsPerItem))
	}
	if err := h.checkQuota(ctx, tx, userID, pending); err != nil {
		return "", nil, err
	}

	for _, p := range pending {
		keys, err := h.storeFile(ctx, tx, userID, entryID, p)
		uploaded = append(uploaded, keys...)
		if err != nil {
			return "", nil, err
		}
	}

	entry, err := loadEntry(ctx, tx, userID, entryID)
	if err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	committed = true
	return status, &entry, nil
}

// checkVersion locks the entry row and rejects missing entries and stale versions (0 skips the check).
func checkVersion(ctx context.Context, tx *sql.Tx, userID, entryID string, version int) error {
	var current int
	var deletedAt *time.Time
	err := tx.QueryRowContext(ctx, `
		SELECT version, deleted_at FROM mood_entries WHERE entry_id = $1 AND user_id = $2 FOR UPDATE
	`, entryID, userID).Scan(&current, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedAt != nil) {
		return &opError{status: statusNotFound, msg: "entry not found"}
	}
	if err != nil {
		return err
	}
	if version != 0 && version != current {
		entry, err := loadEntry(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}
		return &opError{status: statusConflict, msg: "stale version", entry: &entry}
	}
	return nil
}

// checkQuota counts files only and refuses a preview larger than its file, so storage stays under 2x the quota.
func (h *Handler) checkQuota(ctx context.Context, tx *sql.Tx, userID string, pending []pendingFile) error {
	if h.Settings.StorageQuota <= 0 || len(pending) == 0 {
		return nil
	}
	var incoming int64
	for _, p := range pending {
		if p.previewSize() > p.header.Size {
			return invalid("a preview is larger than its file")
		}
		incoming += p.header.Size
	}
	used, err := storageUsed(ctx, tx, userID)
	if err != nil {
		return err
	}
	if used+incoming > h.Settings.StorageQuota {
		return &opError{status: statusQuotaExceeded, msg: "storage quota exceeded"}
	}
	return nil
}

// storeFile uploads a file and its preview, then records them; it returns the keys it uploaded.
func (h *Handler) storeFile(ctx context.Context, tx *sql.Tx, userID, entryID string, p pendingFile) ([]string, error) {
	var uploaded []string
	key := newObjectKey(userID, entryID)
	if err := h.upload(ctx, key, p.header, p.mediaType); err != nil {
		return uploaded, err
	}
	uploaded = append(uploaded, key)

	var previewKey *string
	if p.preview != nil {
		k := key + "-preview"
		if err := h.upload(ctx, k, p.preview, p.mediaType); err != nil {
			return uploaded, err
		}
		uploaded = append(uploaded, k)
		previewKey = &k
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO entry_attachments (entry_id, s3_object_key, preview_object_key, encrypted_aes_key, media_type, size_bytes, preview_size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, entryID, key, previewKey, p.aesKey, p.mediaType, p.header.Size, p.previewSize())
	return uploaded, err
}

func (h *Handler) upload(ctx context.Context, key string, fh *multipart.FileHeader, mediaType string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	return h.Storage.Upload(ctx, key, src, fh.Size, mediaType)
}

// Objects deleted before commit: a storage failure rolls back so the client can retry.
func (h *Handler) deleteEntry(ctx context.Context, userID string, op models.EntryOperation) (string, error) {
	if err := uuid.Validate(op.EntryID); err != nil {
		return "", invalid("entry_id must be a UUID")
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	seq, err := nextSeq(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	if err := checkVersion(ctx, tx, userID, op.EntryID, op.Version); err != nil {
		return "", err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE mood_entries
		SET mood_scale = '', metadata_blob = '', sha256_hash = '', version = version + 1, seq = $1,
			updated_at = NOW(), deleted_at = NOW()
		WHERE entry_id = $2 AND user_id = $3
	`, seq, op.EntryID, userID)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_attachments WHERE entry_id = $1`, op.EntryID); err != nil {
		return "", err
	}
	if err := tombstoneWhere(ctx, tx, "bookmarks", userID, "entry_id", op.EntryID); err != nil {
		return "", err
	}
	if err := h.Storage.DeletePrefix(ctx, userID+"/"+op.EntryID+"/"); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return statusDeleted, nil
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
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
