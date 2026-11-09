package main

import (
	"cloud/handlers"
	"cloud/services"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

// run dispatches subcommands; "serve" is the default. "pair" is reserved for self-hosted pairing.
func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve()
	case "pair":
		return errors.New("pairing is not available yet")
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (use serve, pair or version)", cmd)
	}
}

func serve() error {
	_ = godotenv.Load()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.PostgresDSN())
	if err != nil {
		return fmt.Errorf("database connection configuration error: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	storage, err := services.NewS3Storage(ctx, cfg.S3)
	cancel()
	if err != nil {
		return fmt.Errorf("object storage init error: %w", err)
	}

	mailer, err := services.NewMailer(cfg.MailDriver, cfg.MailFrom)
	if err != nil {
		return err
	}

	authService := services.NewAuthService()
	h := handlers.NewHandler(db, &services.MockStripeService{}, authService, storage, mailer, handlers.Settings{
		Version:      version,
		Mode:         cfg.Mode,
		StorageQuota: cfg.StorageQuota,
		ServerSecret: serverSecret(cfg.ServerSecret),
	})

	router, err := newRouter(h, db, authService, cfg.TrustedProxies)
	if err != nil {
		return err
	}
	return router.Run(":8080")
}

// serverSecret falls back to a random value, which changes fake login salts on each restart.
func serverSecret(configured string) []byte {
	if configured != "" {
		return []byte(configured)
	}
	log.Println("SERVER_SECRET is not set; using a random one for this run")
	b := make([]byte, 32)
	rand.Read(b)
	return b
}
