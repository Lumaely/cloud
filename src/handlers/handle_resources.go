package handlers

import (
	"cloud/src/models"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Resource describes one encrypted per-feature collection (patterns, bookmarks, ...).
type Resource struct {
	Path  string
	Table string
	Refs  []Ref
	// OnDelete runs inside the tombstone transaction.
	OnDelete func(ctx context.Context, tx *sql.Tx, userID, id string) error
}

// Ref is a plaintext link to another row of the same user; Nullable refs may be null.
type Ref struct {
	Column   string
	Table    string
	Nullable bool
	get      func(*models.RecordInput) *string
	set      func(*models.Record, *string)
}

var (
	refEntry = Ref{Column: "entry_id", Table: "mood_entries",
		get: func(in *models.RecordInput) *string { return in.EntryID },
		set: func(r *models.Record, v *string) { r.EntryID = v }}
	refFolder = Ref{Column: "folder_id", Table: "bookmark_folders", Nullable: true,
		get: func(in *models.RecordInput) *string { return in.FolderID },
		set: func(r *models.Record, v *string) { r.FolderID = v }}
)

func Resources() []Resource {
	return []Resource{
		{Path: "patterns", Table: "patterns"},
		{Path: "activities", Table: "activities"},
		{Path: "places", Table: "places"},
		{Path: "bookmark-folders", Table: "bookmark_folders", OnDelete: moveBookmarksToUnsorted},
		{Path: "bookmarks", Table: "bookmarks", Refs: []Ref{refEntry, refFolder}},
	}
}

func (r Resource) columns() string {
	cols := []string{"id", "blob", "version", "seq", "updated_at", "deleted_at"}
	for _, ref := range r.Refs {
		cols = append(cols, ref.Column)
	}
	return strings.Join(cols, ", ")
}

func (r Resource) scan(row interface{ Scan(...any) error }) (models.Record, error) {
	var rec models.Record
	refs := make([]*string, len(r.Refs))
	dest := []any{&rec.ID, &rec.Blob, &rec.Version, &rec.Seq, &rec.UpdatedAt, &rec.DeletedAt}
	for i := range refs {
		dest = append(dest, &refs[i])
	}
	if err := row.Scan(dest...); err != nil {
		return rec, err
	}
	for i, ref := range r.Refs {
		ref.set(&rec, refs[i])
	}
	return rec, nil
}

func (r Resource) load(ctx context.Context, q queryer, userID, id string) (models.Record, error) {
	return r.scan(q.QueryRowContext(ctx, `SELECT `+r.columns()+` FROM `+r.Table+` WHERE id = $1 AND user_id = $2`, id, userID))
}

func (h *Handler) ListResource(r Resource) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, err := parsePage(c)
		if err != nil {
			badRequest(c, err)
			return
		}
		ctx := c.Request.Context()
		userID := c.GetString("userID")

		rows, err := h.DB.QueryContext(ctx, `
			SELECT `+r.columns()+` FROM `+r.Table+`
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
			rec, err := r.scan(rows)
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

		page, err := pageMeta(ctx, h.DB, r.Table, userID, p, items, func(rec models.Record) int64 { return rec.Seq })
		if err != nil {
			serverError(c, "Database query failed")
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

// bindRecord parses the body and checks refs point to live rows of the same user.
func (h *Handler) bindRecord(c *gin.Context, tx *sql.Tx, r Resource, userID string) (models.RecordInput, []any, bool) {
	var in models.RecordInput
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 2*maxBlobBytes)).Decode(&in); err != nil {
		badRequest(c, errors.New("invalid JSON body"))
		return in, nil, false
	}
	if len(in.Blob) == 0 || len(in.Blob) > maxBlobBytes {
		badRequest(c, fmt.Errorf("blob must be 1 to %d bytes", maxBlobBytes))
		return in, nil, false
	}

	values := make([]any, len(r.Refs))
	for i, ref := range r.Refs {
		v := ref.get(&in)
		if v == nil || *v == "" {
			if !ref.Nullable {
				badRequest(c, fmt.Errorf("%s is required", ref.Column))
				return in, nil, false
			}
			continue
		}
		id, ok := canonicalUUID(*v)
		if !ok {
			badRequest(c, fmt.Errorf("%s must be a UUID", ref.Column))
			return in, nil, false
		}
		*v = id
		idCol := "id"
		if ref.Table == "mood_entries" {
			idCol = "entry_id"
		}
		var exists bool
		err := tx.QueryRowContext(c.Request.Context(), `
			SELECT EXISTS (SELECT 1 FROM `+ref.Table+` WHERE `+idCol+` = $1 AND user_id = $2 AND deleted_at IS NULL)
		`, *v, userID).Scan(&exists)
		if err != nil {
			serverError(c, "Database error")
			return in, nil, false
		}
		if !exists {
			badRequest(c, fmt.Errorf("%s does not exist", ref.Column))
			return in, nil, false
		}
		values[i] = *v
	}
	return in, values, true
}

func (h *Handler) CreateResource(r Resource) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		in, refs, ok := h.bindRecord(c, tx, r, userID)
		if !ok {
			return
		}

		cols := []string{"user_id", "blob", "seq"}
		args := []any{userID, in.Blob, seq}
		for i, ref := range r.Refs {
			cols = append(cols, ref.Column)
			args = append(args, refs[i])
		}
		placeholders := make([]string, len(args))
		for i := range args {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		var id string
		err = tx.QueryRowContext(ctx, `INSERT INTO `+r.Table+` (`+strings.Join(cols, ", ")+`) VALUES (`+strings.Join(placeholders, ", ")+`) RETURNING id`, args...).Scan(&id)
		if err != nil {
			serverError(c, "Database error")
			return
		}
		h.commitRecord(c, tx, r, userID, id, http.StatusCreated)
	}
}

func (h *Handler) UpdateResource(r Resource) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		userID := c.GetString("userID")
		id, ok := canonicalUUID(c.Param("id"))
		if !ok {
			badRequest(c, errors.New("id must be a UUID"))
			return
		}
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
		in, refs, ok := h.bindRecord(c, tx, r, userID)
		if !ok {
			return
		}
		current, ok := h.lockRecord(c, tx, r, userID, id)
		if !ok {
			return
		}
		if in.Version != current.Version {
			c.JSON(http.StatusConflict, gin.H{"error": "Stale version", "current": current})
			return
		}

		set := []string{"blob = $3", "seq = $4", "version = version + 1", "updated_at = NOW()"}
		args := []any{id, userID, in.Blob, seq}
		for i, ref := range r.Refs {
			args = append(args, refs[i])
			set = append(set, fmt.Sprintf("%s = $%d", ref.Column, len(args)))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+r.Table+` SET `+strings.Join(set, ", ")+` WHERE id = $1 AND user_id = $2`, args...); err != nil {
			serverError(c, "Database error")
			return
		}
		h.commitRecord(c, tx, r, userID, id, http.StatusOK)
	}
}

func (h *Handler) DeleteResource(r Resource) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		userID := c.GetString("userID")
		id, ok := canonicalUUID(c.Param("id"))
		if !ok {
			badRequest(c, errors.New("id must be a UUID"))
			return
		}
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
		if _, ok := h.lockRecord(c, tx, r, userID, id); !ok {
			return
		}
		if err := tombstone(ctx, tx, r.Table, userID, id, seq); err != nil {
			serverError(c, "Database error")
			return
		}
		if r.OnDelete != nil {
			if err := r.OnDelete(ctx, tx, userID, id); err != nil {
				serverError(c, "Database error")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			serverError(c, "Transaction commit failed")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// lockRecord locks a live record; it answers 404 itself when there is none.
func (h *Handler) lockRecord(c *gin.Context, tx *sql.Tx, r Resource, userID, id string) (models.Record, bool) {
	rec, err := r.scan(tx.QueryRowContext(c.Request.Context(), `SELECT `+r.columns()+` FROM `+r.Table+` WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && rec.DeletedAt != nil) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return rec, false
	}
	if err != nil {
		serverError(c, "Database error")
		return rec, false
	}
	return rec, true
}

func (h *Handler) commitRecord(c *gin.Context, tx *sql.Tx, r Resource, userID, id string, status int) {
	rec, err := r.load(c.Request.Context(), tx, userID, id)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(c, "Transaction commit failed")
		return
	}
	c.JSON(status, rec)
}

func tombstone(ctx context.Context, tx *sql.Tx, table, userID, id string, seq int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE `+table+`
		SET blob = '', version = version + 1, seq = $3, updated_at = $4, deleted_at = $4
		WHERE id = $1 AND user_id = $2
	`, id, userID, seq, time.Now())
	return err
}

// tombstoneWhere tombstones every live row matching column = value, one seq each.
func tombstoneWhere(ctx context.Context, tx *sql.Tx, table, userID, column, value string) error {
	ids, err := liveIDs(ctx, tx, table, userID, column, value)
	if err != nil {
		return err
	}
	for _, id := range ids {
		seq, err := nextSeq(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := tombstone(ctx, tx, table, userID, id, seq); err != nil {
			return err
		}
	}
	return nil
}

func moveBookmarksToUnsorted(ctx context.Context, tx *sql.Tx, userID, folderID string) error {
	ids, err := liveIDs(ctx, tx, "bookmarks", userID, "folder_id", folderID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		seq, err := nextSeq(ctx, tx, userID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE bookmarks SET folder_id = NULL, version = version + 1, seq = $3, updated_at = NOW()
			WHERE id = $1 AND user_id = $2
		`, id, userID, seq)
		if err != nil {
			return err
		}
	}
	return nil
}

func liveIDs(ctx context.Context, tx *sql.Tx, table, userID, column, value string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table+` WHERE user_id = $1 AND `+column+` = $2 AND deleted_at IS NULL ORDER BY id`, userID, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
