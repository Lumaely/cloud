package app

import (
	"cloud/src/handlers"
	"cloud/src/middleware"
	"cloud/src/services"
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
)

func NewRouter(h *handlers.Handler, db *sql.DB, auth *services.AuthService, trustedProxies []string) (*gin.Engine, error) {
	r := gin.Default()
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		return nil, err
	}

	authLimit := middleware.RateLimit(middleware.NewRateLimiter(10, time.Minute))
	feedbackLimit := middleware.RateLimit(middleware.NewRateLimiter(10, time.Minute))

	r.GET("/health", h.HealthCheck)
	r.POST("/feedback", feedbackLimit, h.SendFeedback)

	users := r.Group("/users")
	users.POST("/register", authLimit, h.RegisterUser)
	users.POST("/login/params", authLimit, h.LoginParams)
	users.POST("/login", authLimit, h.Login)
	users.POST("/recover/start", authLimit, h.RecoverStart)
	users.POST("/recover/finish", authLimit, h.RecoverFinish)
	if h.Settings.Mode == handlers.ModeCloud {
		users.POST("/register/start", authLimit, h.RegisterStart)
		users.POST("/erase/request", authLimit, h.EraseRequest)
		users.POST("/erase/confirm", authLimit, h.EraseConfirm)
	}

	protected := r.Group("")
	protected.Use(middleware.AuthRequired(db, auth))
	{
		protected.GET("/users/me", h.GetUserProfile)
		protected.DELETE("/users/me", h.DeleteUserProfile)
		if h.Settings.Mode == handlers.ModeCloud {
			protected.POST("/users/me/delete/request", h.DeleteRequest)
		}
		protected.GET("/users/me/keys", h.GetKeys)
		protected.PUT("/users/me/password", h.ChangePassword)
		protected.GET("/users/me/sessions", h.ListSessions)
		protected.DELETE("/users/me/sessions", h.SignOutAllDevices)
		protected.DELETE("/users/me/sessions/:session_id", h.SignOutDevice)
		protected.DELETE("/users/me/diary", h.ResetDiary)
		protected.GET("/users/me/storage", h.GetStorage)

		protected.GET("/changes", h.ListChanges)
		protected.POST("/changes", h.PushChanges)
		for _, col := range handlers.Collections {
			protected.PUT("/"+col+"/:id", h.PutRecord(col))
			protected.DELETE("/"+col+"/:id", h.DeleteRecord(col))
		}

		protected.GET("/attachments", h.ListAttachments)
		protected.PUT("/attachments/:attachment_id", h.UploadAttachment)
		protected.GET("/attachments/:attachment_id", h.DownloadAttachment)
		protected.DELETE("/attachments/:attachment_id", h.DeleteAttachment)
		protected.PUT("/attachments/:attachment_id/preview", h.UploadPreview)
		protected.GET("/attachments/:attachment_id/preview", h.DownloadAttachmentPreview)
	}

	return r, nil
}
