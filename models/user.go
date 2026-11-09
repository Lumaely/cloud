package models

import (
	"encoding/json"
	"time"
)

type User struct {
	UserID     string    `json:"user_id"`
	Email      *string   `json:"email"`
	DiaryEpoch int       `json:"diary_epoch"`
	CreatedAt  time.Time `json:"created_at"`
}

// KeyMaterial is what the password seals; see docs/API.md.
type KeyMaterial struct {
	KDFSalt            []byte          `json:"kdf_salt" binding:"required"`
	KDFParams          json.RawMessage `json:"kdf_params" binding:"required"`
	WrappedKeyPassword []byte          `json:"wrapped_key_password" binding:"required"`
}

type RecoveryMaterial struct {
	RecoveryAuthKey      []byte `json:"recovery_auth_key" binding:"required"`
	WrappedKeyRecovery   []byte `json:"wrapped_key_recovery" binding:"required"`
	EncryptedRecoveryKey []byte `json:"encrypted_recovery_key" binding:"required"`
}

type KeyBundle struct {
	KDFSalt              []byte          `json:"kdf_salt"`
	KDFParams            json.RawMessage `json:"kdf_params"`
	WrappedKeyPassword   []byte          `json:"wrapped_key_password"`
	WrappedKeyRecovery   []byte          `json:"wrapped_key_recovery"`
	EncryptedRecoveryKey []byte          `json:"encrypted_recovery_key"`
}

type RegisterRequest struct {
	Email   string `json:"email" binding:"required,email"`
	AuthKey []byte `json:"auth_key" binding:"required"`
	KeyMaterial
	RecoveryMaterial
}

type LoginParamsRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type LoginParamsResponse struct {
	KDFSalt   []byte          `json:"kdf_salt"`
	KDFParams json.RawMessage `json:"kdf_params"`
}

type LoginRequest struct {
	Email   string `json:"email" binding:"required,email"`
	AuthKey []byte `json:"auth_key" binding:"required"`
}

type AuthResponse struct {
	Token string    `json:"token"`
	User  User      `json:"user"`
	Keys  KeyBundle `json:"keys"`
}

type RecoverStartRequest struct {
	Email           string `json:"email" binding:"required,email"`
	RecoveryAuthKey []byte `json:"recovery_auth_key" binding:"required"`
}

type RecoverStartResponse struct {
	RecoveryToken      string `json:"recovery_token"`
	WrappedKeyRecovery []byte `json:"wrapped_key_recovery"`
}

type RecoverFinishRequest struct {
	RecoveryToken string `json:"recovery_token" binding:"required"`
	AuthKey       []byte `json:"auth_key" binding:"required"`
	KeyMaterial
}

type ChangePasswordRequest struct {
	CurrentAuthKey      []byte            `json:"current_auth_key" binding:"required"`
	AuthKey             []byte            `json:"auth_key" binding:"required"`
	Recovery            *RecoveryMaterial `json:"recovery"`
	SignOutOtherDevices bool              `json:"sign_out_other_devices"`
	KeyMaterial
}

type EraseRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type EraseConfirmRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Code    string `json:"code" binding:"required"`
	AuthKey []byte `json:"auth_key" binding:"required"`
	KeyMaterial
	RecoveryMaterial
}

type StorageUsage struct {
	UsedBytes  int64 `json:"used_bytes"`
	QuotaBytes int64 `json:"quota_bytes"`
}
