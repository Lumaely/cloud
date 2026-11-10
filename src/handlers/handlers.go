package handlers

import (
	"cloud/src/services"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	ModeCloud      = "cloud"
	ModeSelfHosted = "self-hosted"
)

// Settings are the server-wide values handlers depend on.
type Settings struct {
	Version      string
	Mode         string
	StorageQuota int64 // bytes; 0 = unlimited
	ServerSecret []byte
}

type Handler struct {
	DB       *sql.DB
	Payment  services.PaymentService
	Auth     *services.AuthService
	Storage  services.BlobStorage
	Mailer   services.Mailer
	Settings Settings
}

func NewHandler(db *sql.DB, payment services.PaymentService, auth *services.AuthService, storage services.BlobStorage, mailer services.Mailer, settings Settings) *Handler {
	return &Handler{
		DB:       db,
		Payment:  payment,
		Auth:     auth,
		Storage:  storage,
		Mailer:   mailer,
		Settings: settings,
	}
}

func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "version": h.Settings.Version, "mode": h.Settings.Mode})
}
