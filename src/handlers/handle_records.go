package handlers

import (
	"cloud/src/models"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	maxChanges    = 100
	recordColumns = `collection, id, blob, version, seq, updated_at, deleted_at`

	statusCreated  = "created"
	statusUpdated  = "updated"
	statusDeleted  = "deleted"
	statusConflict = "conflict"
	statusNotFound = "not_found"
	statusInvalid  = "invalid"
	statusError    = "error"
)

// Collections are the synced record types; each is served at /{collection}/{id}.
var Collections = []string{"entries", "patterns", "activities", "places", "bookmark-folders", "bookmarks"}

// Exact forms only: the app binds the id into each blob's AAD, so the server must store it unchanged.
var recordIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{64}|` + uuidV7Pattern + `)$`)

const uuidV7Pattern = `[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`

func validRecordID(id string) bool {
	return recordIDPattern.MatchString(id)
}

// opError carries the result status of a failed change.
type opError struct {
	status string
	msg    string
	item   *models.Record
}

func (e *opError) Error() string { return e.msg }

func invalid(msg string) *opError { return &opError{status: statusInvalid, msg: msg} }

func validateChange(ch models.Change) error {
	switch {
	case !slices.Contains(Collections, ch.Collection):
		return invalid("unknown collection")
	case !validRecordID(ch.ID):
		return invalid("id must be 64 lowercase hex characters or a lowercase UUIDv7")
	case ch.Version < 0:
		return invalid("version must not be negative")
	case ch.Deleted && ch.Version < 1:
		return invalid("version is required to delete")
	case ch.Deleted && len(ch.Blob) > 0:
		return invalid("a delete carries no blob")
	case !ch.Deleted && (len(ch.Blob) == 0 || len(ch.Blob) > maxBlobBytes):
		return invalid(fmt.Sprintf("blob must be 1 to %d bytes", maxBlobBytes))
	}
	return nil
}

func scanRecord(row interface{ Scan(...any) error }) (models.Record, error) {
	var r models.Record
	err := row.Scan(&r.Collection, &r.ID, &r.Blob, &r.Version, &r.Seq, &r.UpdatedAt, &r.DeletedAt)
	return r, err
}

func loadRecord(ctx context.Context, q queryer, userID, collection, id string) (models.Record, error) {
	return scanRecord(q.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM records WHERE user_id = $1 AND collection = $2 AND id = $3`, userID, collection, id))
}

// applyChange writes one change in its own transaction. Version must equal the server's (0 when unknown):
// a stale write gets a conflict carrying the current record, so an edit is never silently lost.
func (h *Handler) applyChange(ctx context.Context, userID string, ch models.Change) (string, *models.Record, error) {
	if err := validateChange(ch); err != nil {
		return "", nil, err
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	seq, err := nextSeq(ctx, tx, userID)
	if err != nil {
		return "", nil, err
	}
	current, err := scanRecord(tx.QueryRowContext(ctx, `
		SELECT `+recordColumns+` FROM records WHERE user_id = $1 AND collection = $2 AND id = $3 FOR UPDATE
	`, userID, ch.Collection, ch.ID))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}

	var status string
	switch {
	case ch.Deleted && (!found || current.DeletedAt != nil):
		return "", nil, &opError{status: statusNotFound, msg: "record not found"}
	case !found && ch.Version != 0:
		return "", nil, &opError{status: statusNotFound, msg: "record not found"}
	case found && ch.Version != current.Version:
		return "", nil, &opError{status: statusConflict, msg: "stale version", item: &current}
	case ch.Deleted:
		status = statusDeleted
		_, err = tx.ExecContext(ctx, `
			UPDATE records SET blob = '', version = version + 1, seq = $4, updated_at = NOW(), deleted_at = NOW()
			WHERE user_id = $1 AND collection = $2 AND id = $3
		`, userID, ch.Collection, ch.ID, seq)
	case !found:
		status = statusCreated
		_, err = tx.ExecContext(ctx, `
			INSERT INTO records (user_id, collection, id, blob, version, seq) VALUES ($1, $2, $3, $4, 1, $5)
		`, userID, ch.Collection, ch.ID, ch.Blob, seq)
	default:
		status = statusUpdated
		_, err = tx.ExecContext(ctx, `
			UPDATE records SET blob = $4, version = version + 1, seq = $5, updated_at = NOW(), deleted_at = NULL
			WHERE user_id = $1 AND collection = $2 AND id = $3
		`, userID, ch.Collection, ch.ID, ch.Blob, seq)
	}
	if err != nil {
		return "", nil, err
	}

	var item *models.Record
	if !ch.Deleted {
		rec, err := loadRecord(ctx, tx, userID, ch.Collection, ch.ID)
		if err != nil {
			return "", nil, err
		}
		item = &rec
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return status, item, nil
}

func (h *Handler) ListChanges(c *gin.Context) {
	p, err := parsePage(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	// One snapshot: items, total and diary_epoch must describe the same diary, even during a reset.
	tx, err := h.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT `+recordColumns+` FROM records
		WHERE user_id = $1 AND seq > $2
		ORDER BY seq ASC
		LIMIT $3
	`, userID, p.cursor, p.limit+1)
	if err != nil {
		serverError(c, "Database query failed")
		return
	}
	defer rows.Close()

	items := make([]models.Record, 0)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			serverError(c, "Failed to parse records")
			return
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		serverError(c, "Failed to parse records")
		return
	}

	page, err := pageMeta(ctx, tx, "records", userID, p, items, func(rec models.Record) int64 { return rec.Seq })
	if err != nil {
		serverError(c, "Database query failed")
		return
	}
	c.JSON(http.StatusOK, page)
}

// PushChanges applies up to maxChanges changes of any collections; each commits on its own.
func (h *Handler) PushChanges(c *gin.Context) {
	var req models.ChangesRequest
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxChanges*2*maxBlobBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		badRequest(c, errors.New("body must be {\"changes\": [...]}"))
		return
	}
	if len(req.Changes) == 0 || len(req.Changes) > maxChanges {
		badRequest(c, fmt.Errorf("changes must hold 1 to %d items", maxChanges))
		return
	}

	ctx := c.Request.Context()
	userID := c.GetString("userID")
	results := make([]models.ChangeResult, len(req.Changes))
	for i, ch := range req.Changes {
		res := models.ChangeResult{Collection: ch.Collection, ID: ch.ID}
		status, item, err := h.applyChange(ctx, userID, ch)
		var oe *opError
		switch {
		case errors.As(err, &oe):
			res.Status, res.Error, res.Item = oe.status, oe.msg, oe.item
		case err != nil:
			log.Printf("change %s/%s for %s failed: %v", ch.Collection, ch.ID, userID, err)
			res.Status, res.Error = statusError, "internal error"
		default:
			res.Status, res.Item = status, item
		}
		results[i] = res
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

func (h *Handler) PutRecord(collection string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in models.RecordWrite
		if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 2*maxBlobBytes)).Decode(&in); err != nil {
			badRequest(c, errors.New("invalid JSON body"))
			return
		}
		h.respondToChange(c, models.Change{Collection: collection, ID: c.Param("id"), Version: in.Version, Blob: in.Blob})
	}
}

// DeleteRecord takes the version it deletes in ?version=, so a delete never wins over an unseen edit.
func (h *Handler) DeleteRecord(collection string) gin.HandlerFunc {
	return func(c *gin.Context) {
		version, err := strconv.Atoi(c.Query("version"))
		if err != nil {
			badRequest(c, errors.New("version query parameter is required"))
			return
		}
		h.respondToChange(c, models.Change{Collection: collection, ID: c.Param("id"), Version: version, Deleted: true})
	}
}

func (h *Handler) respondToChange(c *gin.Context, ch models.Change) {
	status, item, err := h.applyChange(c.Request.Context(), c.GetString("userID"), ch)
	var oe *opError
	switch {
	case errors.As(err, &oe) && oe.status == statusConflict:
		c.JSON(http.StatusConflict, gin.H{"error": oe.msg, "current": oe.item})
	case errors.As(err, &oe) && oe.status == statusNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": oe.msg})
	case errors.As(err, &oe):
		c.JSON(http.StatusBadRequest, gin.H{"error": oe.msg})
	case err != nil:
		log.Printf("change %s/%s failed: %v", ch.Collection, ch.ID, err)
		serverError(c, "Database error")
	case status == statusDeleted:
		c.Status(http.StatusNoContent)
	case status == statusCreated:
		c.JSON(http.StatusCreated, item)
	default:
		c.JSON(http.StatusOK, item)
	}
}
