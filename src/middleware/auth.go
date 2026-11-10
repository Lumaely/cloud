package middleware

import (
	"cloud/src/services"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

var (
	ErrNoToken      = errors.New("authorization header required")
	ErrBadFormat    = errors.New("format must be Bearer <token>")
	ErrInvalidToken = errors.New("invalid or expired session")
)

// LookupSession resolves an Authorization header to its user and session and refreshes its last_seen_at.
func LookupSession(ctx context.Context, db *sql.DB, auth *services.AuthService, header string) (userID, sessionID string, err error) {
	if header == "" {
		return "", "", ErrNoToken
	}
	parts := strings.Split(header, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", "", ErrBadFormat
	}

	// One round trip; the UPDATE writes only when last_seen_at is over 5 minutes old.
	query := `WITH s AS (SELECT user_id, session_id, last_seen_at FROM user_sessions WHERE token_hash = $1 AND expires_at > NOW()),
		touched AS (UPDATE user_sessions u SET last_seen_at = NOW() FROM s WHERE u.session_id = s.session_id AND s.last_seen_at < NOW() - INTERVAL '5 minutes')
		SELECT user_id, session_id FROM s`
	err = db.QueryRowContext(ctx, query, auth.HashToken(parts[1])).Scan(&userID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrInvalidToken
	}
	return userID, sessionID, err
}

func AuthRequired(db *sql.DB, auth *services.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, sessionID, err := LookupSession(c.Request.Context(), db, auth, c.GetHeader("Authorization"))
		switch {
		case errors.Is(err, ErrNoToken):
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
		case errors.Is(err, ErrBadFormat):
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Format must be Bearer <token>"})
		case errors.Is(err, ErrInvalidToken):
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired session"})
		case err != nil:
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		default:
			c.Set("userID", userID)
			c.Set("sessionID", sessionID)
			c.Next()
		}
	}
}
