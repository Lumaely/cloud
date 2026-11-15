package handlers

import (
	"cloud/src/models"
	"cloud/src/services"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	sessionTTL         = 60 * 24 * time.Hour
	maxSessionsPerUser = 50
	challengeTTL       = 15 * time.Minute
	maxCodeAttempts    = 5
	challengeRecovery  = "recovery"
	defaultKDFParams   = `{"alg":"argon2id","m":65536,"t":3,"p":1}`
	userColumns        = `user_id, email, diary_epoch, created_at`
	keyBundleColumns   = `kdf_salt, kdf_params, wrapped_key_password, wrapped_key_recovery`
	errInvalidLogin    = "Invalid email or password"
	errInvalidRecovery = "Invalid email or recovery key"
)

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// burnTime runs one bcrypt compare so unknown emails cost as much as known ones.
func (h *Handler) burnTime(key []byte) {
	dummyHashOnce.Do(func() {
		dummyHash, _ = h.Auth.HashPassword("lumaely-dummy-key")
	})
	h.Auth.CheckPassword(encodeKey(key), dummyHash)
}

// Keys are 32 random bytes; base64 keeps them under bcrypt's 72-byte input limit.
func encodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

func (h *Handler) hashKey(key []byte) (string, error) {
	return h.Auth.HashPassword(encodeKey(key))
}

func (h *Handler) createSession(ctx context.Context, q queryer, userID string, device *models.Device) (string, error) {
	rawToken, tokenHash, err := h.Auth.GenerateSessionToken()
	if err != nil {
		return "", err
	}
	if device == nil {
		device = &models.Device{}
	}
	var sessionID string
	err = q.QueryRowContext(ctx, `
		INSERT INTO user_sessions (user_id, token_hash, expires_at, device_name, platform)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
		RETURNING session_id
	`, userID, tokenHash, time.Now().Add(sessionTTL), device.Name, device.Platform).Scan(&sessionID)
	if err != nil {
		return "", err
	}
	if err := pruneSessions(ctx, q, userID, sessionID); err != nil {
		return "", err
	}
	return rawToken, nil
}

// pruneSessions drops expired sessions and keeps newID plus the most recently seen others, up to maxSessionsPerUser.
func pruneSessions(ctx context.Context, q queryer, userID, newID string) error {
	_, err := q.ExecContext(ctx, `
		DELETE FROM user_sessions
		WHERE user_id = $1 AND (expires_at <= NOW() OR session_id IN (
			SELECT session_id FROM user_sessions
			WHERE user_id = $1 AND expires_at > NOW() AND session_id <> $3
			ORDER BY last_seen_at DESC, created_at DESC
			OFFSET $2
		))
	`, userID, maxSessionsPerUser-1, newID)
	return err
}

func scanUser(row interface{ Scan(...any) error }, u *models.User, extra ...any) error {
	return row.Scan(append([]any{&u.UserID, &u.Email, &u.DiaryEpoch, &u.CreatedAt}, extra...)...)
}

func loadAccount(ctx context.Context, q queryer, userID string) (models.User, models.KeyBundle, error) {
	var u models.User
	var k models.KeyBundle
	row := q.QueryRowContext(ctx, `SELECT `+userColumns+`, `+keyBundleColumns+` FROM users WHERE user_id = $1`, userID)
	err := scanUser(row, &u, &k.KDFSalt, &k.KDFParams, &k.WrappedKeyPassword, &k.WrappedKeyRecovery)
	return u, k, err
}

// respondWithSession opens a session inside tx, commits, and returns token, user and keys.
// It reports whether the response was sent.
func (h *Handler) respondWithSession(c *gin.Context, tx *sql.Tx, userID string, device *models.Device, status int) bool {
	ctx := c.Request.Context()
	token, err := h.createSession(ctx, tx, userID, device)
	if err != nil {
		serverError(c, "Failed to create session")
		return false
	}
	user, keys, err := loadAccount(ctx, tx, userID)
	if err != nil {
		serverError(c, "Database error")
		return false
	}
	if err := tx.Commit(); err != nil {
		serverError(c, "Transaction commit failed")
		return false
	}
	c.JSON(status, models.AuthResponse{Token: token, User: user, Keys: keys})
	return true
}

// RegisterStart answers 202 for known and unknown addresses alike; both send before answering.
func (h *Handler) RegisterStart(c *gin.Context) {
	var req models.RegisterStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()
	email := normalizeEmail(req.Email)

	var exists bool
	if err := h.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		serverError(c, "Database error")
		return
	}

	err := h.issueCode(ctx, email, codeSignup, func(code string) services.Email {
		if exists {
			return services.Email{
				To:      email,
				Subject: "You already have a Lumaely account",
				Body:    "You already have a Lumaely account with this address. Sign in, or use your recovery key if you forgot your password.",
			}
		}
		return services.Email{
			To:      email,
			Subject: "Your Lumaely sign-up code",
			Body:    fmt.Sprintf("Your Lumaely sign-up code is %s.\nIt expires in 15 minutes. If you didn't ask for it, ignore this email.", code),
		}
	})
	if err != nil {
		log.Printf("sign-up code email to %q failed: %v", email, err)
		sendCodeFailed(c)
		return
	}
	c.Status(http.StatusAccepted)
}

func (h *Handler) RegisterUser(c *gin.Context) {
	var req models.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := firstError(validateKey("auth_key", req.AuthKey), validateKeyMaterial(req.KeyMaterial), validateRecoveryMaterial(req.RecoveryMaterial)); err != nil {
		badRequest(c, err)
		return
	}
	email := normalizeEmail(req.Email)

	authHash, err := h.hashKey(req.AuthKey)
	if err != nil {
		serverError(c, "Failed to hash key")
		return
	}
	recoveryHash, err := h.hashKey(req.RecoveryAuthKey)
	if err != nil {
		serverError(c, "Failed to hash key")
		return
	}
	ctx := c.Request.Context()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	if h.Settings.Mode == ModeCloud {
		// Consumed in tx: a conflict rolls back and leaves the code usable.
		err := h.checkCode(ctx, tx, email, codeSignup, req.Code)
		if errors.Is(err, errCodeInvalid) {
			// Commit keeps the failed attempt counted.
			if err := tx.Commit(); err != nil {
				serverError(c, "Transaction commit failed")
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired code"})
			return
		}
		if err != nil {
			serverError(c, "Database error")
			return
		}
	}

	var userID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (email, auth_hash, recovery_auth_hash, kdf_salt, kdf_params,
			wrapped_key_password, wrapped_key_recovery)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING user_id
	`, email, authHash, recoveryHash, req.KDFSalt, []byte(req.KDFParams),
		req.WrappedKeyPassword, req.WrappedKeyRecovery).Scan(&userID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "User already exists"})
			return
		}
		serverError(c, "Database error")
		return
	}
	h.respondWithSession(c, tx, userID, req.Device, http.StatusCreated)
}

// LoginParams answers unknown emails with a stable fake salt so it can't reveal which accounts exist.
func (h *Handler) LoginParams(c *gin.Context) {
	var req models.LoginParamsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	email := normalizeEmail(req.Email)

	var resp models.LoginParamsResponse
	err := h.DB.QueryRowContext(c.Request.Context(), `SELECT kdf_salt, kdf_params FROM users WHERE email = $1`, email).
		Scan(&resp.KDFSalt, &resp.KDFParams)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, models.LoginParamsResponse{KDFSalt: h.fakeSalt(email), KDFParams: json.RawMessage(defaultKDFParams)})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) fakeSalt(email string) []byte {
	mac := hmac.New(sha256.New, h.Settings.ServerSecret)
	mac.Write([]byte("kdf-salt:" + email))
	return mac.Sum(nil)[:kdfSaltLength]
}

func (h *Handler) Login(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()

	var userID, authHash string
	err := h.DB.QueryRowContext(ctx, `SELECT user_id, auth_hash FROM users WHERE email = $1`, normalizeEmail(req.Email)).
		Scan(&userID, &authHash)
	if errors.Is(err, sql.ErrNoRows) {
		h.burnTime(req.AuthKey)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidLogin})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if !h.Auth.CheckPassword(encodeKey(req.AuthKey), authHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidLogin})
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()
	if h.respondWithSession(c, tx, userID, req.Device, http.StatusOK) {
		if to := h.noticeAddress(ctx, userID); to != "" {
			h.notify(noticeSignIn(to, req.Device, time.Now()))
		}
	}
}

func (h *Handler) RecoverStart(c *gin.Context) {
	var req models.RecoverStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()

	var userID, recoveryHash string
	var wrapped []byte
	err := h.DB.QueryRowContext(ctx, `SELECT user_id, recovery_auth_hash, wrapped_key_recovery FROM users WHERE email = $1`,
		normalizeEmail(req.Email)).Scan(&userID, &recoveryHash, &wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		h.burnTime(req.RecoveryAuthKey)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidRecovery})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if !h.Auth.CheckPassword(encodeKey(req.RecoveryAuthKey), recoveryHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidRecovery})
		return
	}

	token, err := randomHex(32)
	if err != nil {
		serverError(c, "Failed to create recovery token")
		return
	}
	if err := storeChallenge(ctx, h.DB, userID, challengeRecovery, token); err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, models.RecoverStartResponse{RecoveryToken: token, WrappedKeyRecovery: wrapped})
}

func (h *Handler) RecoverFinish(c *gin.Context) {
	var req models.RecoverFinishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := firstError(validateKey("auth_key", req.AuthKey), validateKeyMaterial(req.KeyMaterial)); err != nil {
		badRequest(c, err)
		return
	}
	authHash, err := h.hashKey(req.AuthKey)
	if err != nil {
		serverError(c, "Failed to hash key")
		return
	}

	ctx := c.Request.Context()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM user_challenges
		WHERE kind = $1 AND secret_hash = $2 AND expires_at > NOW()
		RETURNING user_id
	`, challengeRecovery, hashSecret(req.RecoveryToken)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}

	if err := setPasswordKeys(ctx, tx, userID, authHash, req.KeyMaterial); err != nil {
		serverError(c, "Database error")
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID); err != nil {
		serverError(c, "Database error")
		return
	}
	if h.respondWithSession(c, tx, userID, req.Device, http.StatusOK) {
		if to := h.noticeAddress(ctx, userID); to != "" {
			h.notify(noticeRecoveryUsed(to, req.Device, time.Now()))
		}
	}
}

func setPasswordKeys(ctx context.Context, q queryer, userID, authHash string, k models.KeyMaterial) error {
	_, err := q.ExecContext(ctx, `
		UPDATE users SET auth_hash = $2, kdf_salt = $3, kdf_params = $4, wrapped_key_password = $5
		WHERE user_id = $1
	`, userID, authHash, k.KDFSalt, []byte(k.KDFParams), k.WrappedKeyPassword)
	return err
}

func (h *Handler) setRecoveryKeys(ctx context.Context, q queryer, userID string, r models.RecoveryMaterial) error {
	recoveryHash, err := h.hashKey(r.RecoveryAuthKey)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		UPDATE users SET recovery_auth_hash = $2, wrapped_key_recovery = $3
		WHERE user_id = $1
	`, userID, recoveryHash, r.WrappedKeyRecovery)
	return err
}

func (h *Handler) GetUserProfile(c *gin.Context) {
	user, _, err := loadAccount(c.Request.Context(), h.DB, c.GetString("userID"))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *Handler) GetKeys(c *gin.Context) {
	_, keys, err := loadAccount(c.Request.Context(), h.DB, c.GetString("userID"))
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, keys)
}

func (h *Handler) ChangePassword(c *gin.Context) {
	var req models.ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	errs := []error{validateKey("auth_key", req.AuthKey), validateKeyMaterial(req.KeyMaterial)}
	if req.Recovery != nil {
		errs = append(errs, validateRecoveryMaterial(*req.Recovery))
	}
	if err := firstError(errs...); err != nil {
		badRequest(c, err)
		return
	}

	ctx := c.Request.Context()
	userID := c.GetString("userID")
	var currentHash string
	if err := h.DB.QueryRowContext(ctx, `SELECT auth_hash FROM users WHERE user_id = $1`, userID).Scan(&currentHash); err != nil {
		serverError(c, "Database error")
		return
	}
	if !h.Auth.CheckPassword(encodeKey(req.CurrentAuthKey), currentHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Current password is wrong"})
		return
	}
	authHash, err := h.hashKey(req.AuthKey)
	if err != nil {
		serverError(c, "Failed to hash key")
		return
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	if err := setPasswordKeys(ctx, tx, userID, authHash, req.KeyMaterial); err != nil {
		serverError(c, "Database error")
		return
	}
	if req.Recovery != nil {
		if err := h.setRecoveryKeys(ctx, tx, userID, *req.Recovery); err != nil {
			serverError(c, "Database error")
			return
		}
	}
	if req.SignOutOtherDevices {
		_, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = $1 AND session_id <> $2`, userID, c.GetString("sessionID"))
		if err != nil {
			serverError(c, "Database error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(c, "Transaction commit failed")
		return
	}
	c.Status(http.StatusNoContent)
	if to := h.noticeAddress(ctx, userID); to != "" {
		if req.Recovery != nil {
			h.notify(noticeRecoveryReplaced(to, time.Now()))
		} else {
			h.notify(noticePasswordChanged(to, req.SignOutOtherDevices, time.Now()))
		}
	}
}

func (h *Handler) SignOutAllDevices(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("userID")
	if _, err := h.DB.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID); err != nil {
		serverError(c, "Database error")
		return
	}
	c.Status(http.StatusNoContent)
	if to := h.noticeAddress(ctx, userID); to != "" {
		h.notify(noticeSignedOutAll(to, time.Now()))
	}
}

func (h *Handler) ListSessions(c *gin.Context) {
	rows, err := h.DB.QueryContext(c.Request.Context(), `
		SELECT session_id, device_name, platform, created_at, last_seen_at, session_id = $2
		FROM user_sessions
		WHERE user_id = $1 AND expires_at > NOW()
		ORDER BY last_seen_at DESC, created_at DESC
	`, c.GetString("userID"), c.GetString("sessionID"))
	if err != nil {
		serverError(c, "Database error")
		return
	}
	defer rows.Close()

	sessions := make([]models.Session, 0)
	for rows.Next() {
		var s models.Session
		if err := rows.Scan(&s.SessionID, &s.DeviceName, &s.Platform, &s.CreatedAt, &s.LastSeenAt, &s.Current); err != nil {
			serverError(c, "Database error")
			return
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, sessions)
}

func (h *Handler) SignOutDevice(c *gin.Context) {
	sessionID, ok := canonicalUUID(c.Param("session_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id must be a UUID"})
		return
	}
	res, err := h.DB.ExecContext(c.Request.Context(), `DELETE FROM user_sessions WHERE session_id = $1 AND user_id = $2`,
		sessionID, c.GetString("userID"))
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if rows, err := res.RowsAffected(); err != nil || rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteUserProfile(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	// Objects first: a storage failure keeps the account so the client can retry.
	if err := h.Storage.DeletePrefix(ctx, userID+"/"); err != nil {
		serverError(c, "Storage delete error")
		return
	}

	res, err := h.DB.ExecContext(ctx, `DELETE FROM users WHERE user_id = $1`, userID)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if rows, err := res.RowsAffected(); err != nil || rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	// Second sweep: an upload committed between the first sweep and the delete lost its row in the cascade.
	if err := h.Storage.DeletePrefix(context.WithoutCancel(ctx), userID+"/"); err != nil {
		log.Printf("objects of deleted user %s not swept: %v", userID, err)
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetStorage(c *gin.Context) {
	used, err := storageUsed(c.Request.Context(), h.DB, c.GetString("userID"))
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.JSON(http.StatusOK, models.StorageUsage{UsedBytes: used, QuotaBytes: h.Settings.StorageQuota})
}

func storageUsed(ctx context.Context, q queryer, userID string) (int64, error) {
	var used int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM attachments WHERE user_id = $1`, userID).Scan(&used)
	return used, err
}

func (h *Handler) ResetDiary(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("userID")

	// Objects first: a storage failure leaves the diary intact so the client can retry.
	if err := h.Storage.DeletePrefix(ctx, userID+"/"); err != nil {
		serverError(c, "Storage delete error")
		return
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	keys, err := eraseDiaryRows(ctx, tx, userID)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(c, "Transaction commit failed")
		return
	}
	h.deleteObjects(keys)
	c.Status(http.StatusNoContent)
	if to := h.noticeAddress(ctx, userID); to != "" {
		h.notify(noticeDiaryReset(to, time.Now()))
	}
}

// eraseDiaryRows deletes every diary row and bumps diary_epoch so other devices drop their copy.
// It returns the object keys of the deleted files: the caller deletes them after commit, which catches
// files uploaded after its earlier prefix sweep.
func eraseDiaryRows(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	if _, err := nextSeq(ctx, tx, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `DELETE FROM attachments WHERE user_id = $1 RETURNING s3_object_key, preview_object_key`, userID)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var key string
		var preview sql.NullString
		if err := rows.Scan(&key, &preview); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
		if preview.Valid {
			keys = append(keys, preview.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET diary_epoch = diary_epoch + 1 WHERE user_id = $1`, userID)
	return keys, err
}

func (h *Handler) EraseRequest(c *gin.Context) {
	var req models.EraseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()
	email := normalizeEmail(req.Email)

	err := h.DB.QueryRowContext(ctx, `SELECT 1 FROM users WHERE email = $1`, email).Scan(new(int))
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		serverError(c, "Database error")
		return
	}
	// Unknown addresses claim a slot too and the send runs after the answer: timing and status never reveal an account.
	code, err := h.claimCode(ctx, email, codeErase)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	if known && code != "" {
		sendCtx := context.WithoutCancel(ctx)
		h.inBackground(func() {
			err := h.sendCode(sendCtx, email, codeErase, code, func(code string) services.Email {
				return services.Email{
					To:      email,
					Subject: "Your Lumaely erase code",
					Body:    fmt.Sprintf("Your code to erase your Lumaely diary and start over is %s.\nIt expires in 15 minutes. If you didn't ask for it, ignore this email.", code),
				}
			})
			if err != nil {
				log.Printf("erase code email to %q failed: %v", email, err)
			}
		})
	}
	c.Status(http.StatusAccepted)
}

func (h *Handler) EraseConfirm(c *gin.Context) {
	var req models.EraseConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := firstError(validateKey("auth_key", req.AuthKey), validateKeyMaterial(req.KeyMaterial), validateRecoveryMaterial(req.RecoveryMaterial)); err != nil {
		badRequest(c, err)
		return
	}
	ctx := c.Request.Context()
	email := normalizeEmail(req.Email)

	var userID string
	err := h.DB.QueryRowContext(ctx, `SELECT user_id FROM users WHERE email = $1`, email).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired code"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}

	// The code is consumed inside tx: a later failure rolls back and leaves it usable.
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		serverError(c, "Transaction initiation failed")
		return
	}
	defer tx.Rollback()

	err = h.checkCode(ctx, tx, email, codeErase, req.Code)
	if errors.Is(err, errCodeInvalid) {
		// Commit keeps the failed attempt counted.
		if err := tx.Commit(); err != nil {
			serverError(c, "Transaction commit failed")
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired code"})
		return
	}
	if err != nil {
		serverError(c, "Database error")
		return
	}

	authHash, err := h.hashKey(req.AuthKey)
	if err != nil {
		serverError(c, "Failed to hash key")
		return
	}
	if err := h.Storage.DeletePrefix(ctx, userID+"/"); err != nil {
		serverError(c, "Storage delete error")
		return
	}

	var erasedKeys []string
	steps := []func() error{
		func() error {
			var err error
			erasedKeys, err = eraseDiaryRows(ctx, tx, userID)
			return err
		},
		func() error { return setPasswordKeys(ctx, tx, userID, authHash, req.KeyMaterial) },
		func() error { return h.setRecoveryKeys(ctx, tx, userID, req.RecoveryMaterial) },
		func() error {
			_, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID)
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			serverError(c, "Database error")
			return
		}
	}
	h.respondWithSession(c, tx, userID, req.Device, http.StatusOK)
	if c.Writer.Status() == http.StatusOK {
		h.deleteObjects(erasedKeys)
	}
}

func storeChallenge(ctx context.Context, q queryer, userID, kind, secret string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_challenges (user_id, kind, secret_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, kind) DO UPDATE
		SET secret_hash = EXCLUDED.secret_hash, attempts = 0, expires_at = EXCLUDED.expires_at, created_at = NOW()
	`, userID, kind, hashSecret(secret), time.Now().Add(challengeTTL))
	return err
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomDigits(n int) (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", n, v), nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
