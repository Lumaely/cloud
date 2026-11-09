package handlers

import (
	"cloud/middleware"
	"cloud/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

// SendFeedback is public; a valid session links the feedback to its user.
func (h *Handler) SendFeedback(c *gin.Context) {
	var req models.FeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}

	var userID *string
	if header := c.GetHeader("Authorization"); header != "" {
		id, _, err := middleware.LookupSession(c.Request.Context(), h.DB, h.Auth, header)
		if err == nil {
			userID = &id
		}
	}

	_, err := h.DB.ExecContext(c.Request.Context(), `
		INSERT INTO feedback (user_id, message, contact_email, app_version, platform)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''))
	`, userID, req.Message, normalizeEmail(req.ContactEmail), req.AppVersion, req.Platform)
	if err != nil {
		serverError(c, "Database error")
		return
	}
	c.Status(http.StatusCreated)
}
