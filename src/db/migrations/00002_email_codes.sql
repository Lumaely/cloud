-- +goose Up
-- Emailed one-time codes, keyed by address so they also serve accounts not created yet.
CREATE TABLE IF NOT EXISTS email_codes (
    email TEXT NOT NULL,
    kind TEXT NOT NULL,
    secret_hash VARCHAR(64) NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    -- NULL until the email is sent; drives the 60 s resend throttle.
    sent_at TIMESTAMPTZ NULL,
    PRIMARY KEY (email, kind)
);

-- user_challenges keeps only recovery tokens.
DELETE FROM user_challenges WHERE kind = 'erase';

-- +goose Down
DROP TABLE IF EXISTS email_codes;
