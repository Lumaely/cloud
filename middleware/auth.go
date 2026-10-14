package middleware

import (
	"cloud/services"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func AuthRequired(db *sql.DB, auth *services.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
			return
		}

		parts := strings.Split(header, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Format must be Bearer <token>"})
			return
		}

		tokenHash := auth.HashToken(parts[1])

		var userID string
		query := `
			SELECT user_id 
			FROM user_sessions 
			WHERE token_hash = $1 AND expires_at > NOW()
		`
		err := db.QueryRowContext(c.Request.Context(), query, tokenHash).Scan(&userID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired session"})
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		c.Set("userID", userID)
		c.Next()
	}
}
