package main

import (
	"cloud/handlers"
	"cloud/middleware"
	"cloud/services"
	"database/sql"

	"github.com/gin-gonic/gin"
)

func newRouter(h *handlers.Handler, db *sql.DB, auth *services.AuthService) *gin.Engine {
	r := gin.Default()

	r.GET("/health", h.HealthCheck)
	r.POST("/users/register", h.RegisterUser)
	r.POST("/users/login", h.Login)

	protected := r.Group("")
	protected.Use(middleware.AuthRequired(db, auth))
	{
		protected.GET("/users/me", h.GetUserProfile)
		protected.DELETE("/users/me", h.DeleteUserProfile)
		protected.GET("/sync", h.FetchDelta)
		protected.POST("/sync", h.PushChanges)
		protected.GET("/attachments/:attachment_id", h.DownloadAttachment)
	}

	return r
}
