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
    -- Per-user change counter; its row lock orders a user's writes.
    change_seq BIGINT NOT NULL DEFAULT 0,
    diary_epoch INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_sessions (
    session_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    device_name TEXT NULL,
    platform TEXT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Refreshed at most every 5 minutes by middleware.LookupSession.
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Short-lived single-use recovery tokens.
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

-- Emailed one-time codes, keyed by address so they also serve accounts not created yet.
CREATE TABLE IF NOT EXISTS email_codes (
    email TEXT NOT NULL,
    kind TEXT NOT NULL,
    -- HMAC of the code; NULL once burned by too many wrong guesses.
    secret_hash VARCHAR(64) NULL,
    attempts INT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    -- Set when a send slot is claimed, reset to NULL after a failed send; drives the 60 s resend throttle.
    sent_at TIMESTAMPTZ NULL,
    -- Fixed 24 h window capping codes sent per (email, kind).
    window_start TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    window_count INT NOT NULL DEFAULT 0,
    PRIMARY KEY (email, kind)
);

-- Every synced collection. The app chooses ids and encrypts blobs; the server only orders and versions them.
CREATE TABLE IF NOT EXISTS records (
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    collection TEXT NOT NULL,
    id TEXT NOT NULL,
    blob BYTEA NOT NULL,
    version INT NOT NULL,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL,
    PRIMARY KEY (user_id, collection, id)
);

-- Files are not linked to records: the entry blob lists its attachment ids.
CREATE TABLE IF NOT EXISTS attachments (
    user_id UUID NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    attachment_id UUID NOT NULL,
    s3_object_key TEXT NOT NULL,
    preview_object_key TEXT NULL,
    -- File only: the quota counts it, not the preview.
    size_bytes BIGINT NOT NULL,
    preview_size_bytes BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, attachment_id)
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
CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON user_sessions(user_id, last_seen_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_records_seq ON records(user_id, seq);

-- +goose Down
DROP TABLE IF EXISTS feedback, attachments, records, email_codes, user_challenges, user_sessions, users;
