package models

import "time"

type MoodEntry struct {
	EntryID        string       `json:"entry_id"`
	MoodScale      []byte       `json:"mood_scale"`
	MetadataBlob   []byte       `json:"metadata_blob"`
	EntryTimestamp time.Time    `json:"entry_timestamp"`
	SHA256Hash     string       `json:"sha256_hash"`
	Version        int          `json:"version"`
	Seq            int64        `json:"seq"`
	UpdatedAt      time.Time    `json:"updated_at"`
	DeletedAt      *time.Time   `json:"deleted_at,omitempty"`
	Attachments    []Attachment `json:"attachments"`
}

// Attachment file is served by GET /attachments/:attachment_id.
type Attachment struct {
	AttachmentID    string `json:"attachment_id"`
	EncryptedAESKey []byte `json:"encrypted_aes_key"`
	MediaType       string `json:"media_type"`
	SizeBytes       int64  `json:"size_bytes"`
	HasPreview      bool   `json:"has_preview"`
}

type Page[T any] struct {
	Items      []T   `json:"items"`
	NextCursor int64 `json:"next_cursor"`
	HasMore    bool  `json:"has_more"`
	Total      int64 `json:"total"`
	DiaryEpoch int   `json:"diary_epoch"`
}

// EntryOperation is one item of the POST /sync "operations" part.
type EntryOperation struct {
	Ref            string                `json:"ref"`
	EntryID        string                `json:"entry_id"`
	Version        int                   `json:"version"`
	Deleted        bool                  `json:"deleted"`
	MoodScale      []byte                `json:"mood_scale"`
	MetadataBlob   []byte                `json:"metadata_blob"`
	EntryTimestamp string                `json:"entry_timestamp"`
	SHA256Hash     string                `json:"sha256_hash"`
	Attachments    []AttachmentOperation `json:"attachments"`
}

type AttachmentOperation struct {
	File            string `json:"file"`
	Preview         string `json:"preview"`
	EncryptedAESKey []byte `json:"encrypted_aes_key"`
	MediaType       string `json:"media_type"`
}

type OperationResult struct {
	Ref    string     `json:"ref"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Entry  *MoodEntry `json:"entry,omitempty"`
}

type Record struct {
	ID        string     `json:"id"`
	Blob      []byte     `json:"blob"`
	Version   int        `json:"version"`
	Seq       int64      `json:"seq"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Bookmarks only.
	EntryID  *string `json:"entry_id,omitempty"`
	FolderID *string `json:"folder_id,omitempty"`
}

type RecordInput struct {
	Blob     []byte  `json:"blob"`
	Version  int     `json:"version"`
	EntryID  *string `json:"entry_id"`
	FolderID *string `json:"folder_id"`
}

type FeedbackRequest struct {
	Message      string `json:"message" binding:"required,max=5000"`
	ContactEmail string `json:"contact_email" binding:"omitempty,email"`
	AppVersion   string `json:"app_version" binding:"max=64"`
	Platform     string `json:"platform" binding:"max=64"`
}
