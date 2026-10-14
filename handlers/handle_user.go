package handlers

import (
	"cloud/models"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

func (h *Handler) createSession(ctx context.Context, userID string) (string, error) {
	rawToken, tokenHash, err := h.Auth.GenerateSessionToken()
	if err != nil {
		return "", err
	}

	expiresAt := time.Now().Add(60 * 24 * time.Hour)
	query := `
		INSERT INTO user_sessions (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`
	_, err = h.DB.ExecContext(ctx, query, userID, tokenHash, expiresAt)
	if err != nil {
		return "", err
	}

	return rawToken, nil
}

func (h *Handler) RegisterUser(c *gin.Context) {
	var req models.RegisterUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.OAuthID == nil && req.Password == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Must provide either password or oauth_id"})
		return
	}

	var passwordHash *string
	if req.Password != nil {
		hash, err := h.Auth.HashPassword(*req.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash password"})
			return
		}
		passwordHash = &hash
	}

	stripeID, err := h.Payment.CreateCustomer(req.Email, req.FullName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create payment profile"})
		return
	}

	var user models.User
	query := `
		INSERT INTO users (oauth_id, email, password_hash, stripe_customer_id)
		VALUES ($1, $2, $3, $4)
		RETURNING user_id, oauth_id, email, stripe_customer_id, created_at
	`
	err = h.DB.QueryRowContext(c.Request.Context(), query, req.OAuthID, req.Email, passwordHash, stripeID).Scan(
		&user.UserID,
		&user.OAuthID,
		&user.Email,
		&user.StripeCustomerID,
		&user.CreatedAt,
	)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "User already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	token, err := h.createSession(c.Request.Context(), user.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	c.JSON(http.StatusCreated, models.AuthResponse{
		Token: token,
		User:  user,
	})
}

func (h *Handler) Login(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user models.User
	var passwordHash sql.NullString

	query := `
		SELECT user_id, oauth_id, email, password_hash, stripe_customer_id, created_at
		FROM users
		WHERE email = $1
	`
	err := h.DB.QueryRowContext(c.Request.Context(), query, req.Email).Scan(
		&user.UserID,
		&user.OAuthID,
		&user.Email,
		&passwordHash,
		&user.StripeCustomerID,
		&user.CreatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	if !passwordHash.Valid || !h.Auth.CheckPassword(req.Password, passwordHash.String) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	token, err := h.createSession(c.Request.Context(), user.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	c.JSON(http.StatusOK, models.AuthResponse{
		Token: token,
		User:  user,
	})
}

func (h *Handler) GetUserProfile(c *gin.Context) {
	userID := c.GetString("userID")

	var user models.User
	query := `
		SELECT user_id, oauth_id, email, stripe_customer_id, created_at
		FROM users
		WHERE user_id = $1
	`
	err := h.DB.QueryRowContext(c.Request.Context(), query, userID).Scan(
		&user.UserID,
		&user.OAuthID,
		&user.Email,
		&user.StripeCustomerID,
		&user.CreatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.JSON(http.StatusOK, user)
}

func (h *Handler) DeleteUserProfile(c *gin.Context) {
	userID := c.GetString("userID")

	query := `DELETE FROM users WHERE user_id = $1`
	res, err := h.DB.ExecContext(c.Request.Context(), query, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	rows, err := res.RowsAffected()
	if err != nil || rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	c.Status(http.StatusNoContent)
}
