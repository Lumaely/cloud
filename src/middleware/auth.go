package middleware

import (
	"cloud/src/services"
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

var (
	ErrNoToken      = errors.New("authorization header required")
	ErrBadFormat    = errors.New("format must be Bearer <token>")
	ErrInvalidToken = errors.New("invalid or expired session")
)

// LookupSession resolves an Authorization header to its user and session.
func LookupSession(ctx context.Context, db *sql.DB, auth *services.AuthService, header string) (userID, sessionID string, err error) {
	if header == "" {
		return "", "", ErrNoToken
	}
	parts := strings.Split(header, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", "", ErrBadFormat
	}

	query := `
		SELECT user_id, session_id
		FROM user_sessions
		WHERE token_hash = $1 AND expires_at > NOW()
	`
	err = db.QueryRowContext(ctx, query, auth.HashToken(parts[1])).Scan(&userID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrInvalidToken
	}
	return userID, sessionID, err
}

// touchSession is best effort: a failed refresh must not refuse the request.
func touchSession(ctx context.Context, db *sql.DB, sessionID string) {
	_, err := db.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at = NOW() WHERE session_id = $1 AND last_seen_at < NOW() - INTERVAL '5 minutes'`, sessionID)
	if err != nil {
		log.Printf("last_seen_at of session %s not refreshed: %v", sessionID, err)
	}
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
			touchSession(c.Request.Context(), db, sessionID)
			c.Set("userID", userID)
			c.Set("sessionID", sessionID)
			c.Next()
		}
	}
}
