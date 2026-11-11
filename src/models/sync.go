package models

import "time"

// Attachment is a standalone encrypted file; the entry blob that uses it holds its key and media type.
type Attachment struct {
	AttachmentID     string    `json:"attachment_id"`
	SizeBytes        int64     `json:"size_bytes"`
	PreviewSizeBytes int64     `json:"preview_size_bytes"`
	CreatedAt        time.Time `json:"created_at"`
}

type Page[T any] struct {
	Items      []T   `json:"items"`
	NextCursor int64 `json:"next_cursor"`
	HasMore    bool  `json:"has_more"`
	Total      int64 `json:"total"`
	DiaryEpoch int   `json:"diary_epoch"`
}

// Record is one encrypted item of a collection; the app chooses its id.
type Record struct {
	Collection string     `json:"collection"`
	ID         string     `json:"id"`
	Blob       []byte     `json:"blob"`
	Version    int        `json:"version"`
	Seq        int64      `json:"seq"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

// RecordWrite is the body of PUT /{collection}/{id}; Version 0 creates.
type RecordWrite struct {
	Version int    `json:"version"`
	Blob    []byte `json:"blob"`
}

// Change is one item of POST /changes.
type Change struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Version    int    `json:"version"`
	Blob       []byte `json:"blob"`
	Deleted    bool   `json:"deleted"`
}

type ChangesRequest struct {
	Changes []Change `json:"changes"`
}

type ChangeResult struct {
	Collection string  `json:"collection"`
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	Error      string  `json:"error,omitempty"`
	Item       *Record `json:"item,omitempty"`
}

type FeedbackRequest struct {
	Message      string `json:"message" binding:"required,max=5000"`
	ContactEmail string `json:"contact_email" binding:"omitempty,email"`
	AppVersion   string `json:"app_version" binding:"max=64"`
	Platform     string `json:"platform" binding:"max=64"`
}
