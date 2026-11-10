-- +goose Up
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS users (
    user_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL for self-hosted accounts created by pairing.
    email TEXT UNIQUE NULL,
    auth_hash VARCHAR(255) NOT NULL,
    recovery_auth_hash VARCHAR(255) NOT NULL,
    kdf_salt BYTEA NOT NULL,
    kdf_params JSONB NOT NULL,
    wrapped_key_password BYTEA NOT NULL,
    wrapped_key_recovery BYTEA NOT NULL,
    encrypted_recovery_key BYTEA NOT NULL,
    stripe_customer_id TEXT NOT NULL DEFAULT '',
    -- Per-user change counter; its row lock orders a user's writes.
    change_seq BIGINT NOT NULL DEFAULT 0,
    diary_epoch INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_sessions (
    session_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Short-lived single-use codes: password recovery and erase-and-start-over.
CREATE TABLE IF NOT EXISTS user_challenges (
    challenge_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    secret_hash VARCHAR(64) NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, kind)
);

CREATE TABLE IF NOT EXISTS mood_entries (
    entry_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    mood_scale BYTEA NOT NULL,
    metadata_blob BYTEA NOT NULL,
    entry_timestamp TIMESTAMPTZ NOT NULL,
    sha256_hash TEXT NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS entry_attachments (
    attachment_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id UUID NOT NULL REFERENCES mood_entries(entry_id) ON DELETE CASCADE,
    s3_object_key TEXT NOT NULL,
    preview_object_key TEXT NULL,
    encrypted_aes_key BYTEA NOT NULL,
    media_type TEXT NOT NULL,
    -- File plus preview, counted against the storage quota.
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS patterns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    blob BYTEA NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS activities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    blob BYTEA NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS places (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    blob BYTEA NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS bookmark_folders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    blob BYTEA NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS bookmarks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    entry_id UUID NOT NULL REFERENCES mood_entries(entry_id) ON DELETE CASCADE,
    folder_id UUID NULL REFERENCES bookmark_folders(id) ON DELETE SET NULL,
    blob BYTEA NOT NULL,
    version INT NOT NULL DEFAULT 1,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE TABLE IF NOT EXISTS feedback (
    feedback_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NULL REFERENCES users(user_id) ON DELETE SET NULL,
    message TEXT NOT NULL,
    contact_email TEXT NULL,
    app_version TEXT NULL,
    platform TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_token_hash ON user_sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_mood_entries_seq ON mood_entries(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_entry_attachments_entry ON entry_attachments(entry_id);
CREATE INDEX IF NOT EXISTS idx_patterns_seq ON patterns(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_activities_seq ON activities(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_places_seq ON places(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_bookmark_folders_seq ON bookmark_folders(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_bookmarks_seq ON bookmarks(user_id, seq);
CREATE INDEX IF NOT EXISTS idx_bookmarks_entry ON bookmarks(entry_id);
CREATE INDEX IF NOT EXISTS idx_bookmarks_folder ON bookmarks(folder_id);

-- +goose Down
DROP TABLE IF EXISTS feedback, bookmarks, bookmark_folders, places, activities, patterns, entry_attachments, mood_entries, user_challenges, user_sessions, users;
