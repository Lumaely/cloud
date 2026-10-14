package models

import "time"

type MoodEntry struct {
	EntryID        string     `json:"entry_id"`
	MoodScale      []byte     `json:"mood_scale"`
	MetadataBlob   []byte     `json:"metadata_blob"`
	EntryTimestamp time.Time  `json:"entry_timestamp"`
	SHA256Hash     string     `json:"sha256_hash"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}
