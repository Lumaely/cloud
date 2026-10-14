package handlers

import (
	"cloud/services"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	DB      *sql.DB
	Payment services.PaymentService
	Auth    *services.AuthService
	Storage services.BlobStorage
}

func NewHandler(db *sql.DB, payment services.PaymentService, auth *services.AuthService, storage services.BlobStorage) *Handler {
	return &Handler{
		DB:      db,
		Payment: payment,
		Auth:    auth,
		Storage: storage,
	}
}

func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
