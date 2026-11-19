// Package app wires configuration, migrations and HTTP routes into the server commands.
package app

import (
	"cloud/src/handlers"
	"cloud/src/services"
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

// Run dispatches subcommands; "serve" is the default. "pair" is reserved for self-hosted pairing.
func Run(args []string, version string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		_ = godotenv.Load()
		return serve(version)
	case "migrate":
		_ = godotenv.Load()
		return migrateCommand(context.Background(), args[1:], os.Stdout)
	case "pair":
		return errors.New("pairing is not available yet")
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (use serve, migrate, pair or version)", cmd)
	}
}

func serve(version string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.PostgresDSN())
	if err != nil {
		return fmt.Errorf("database connection configuration error: %w", err)
	}
	defer db.Close()

	if err := Migrate(db, cfg.MigrationsDir); err != nil {
		return fmt.Errorf("database migration failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	storage, err := services.NewS3Storage(ctx, cfg.S3)
	cancel()
	if err != nil {
		return fmt.Errorf("object storage init error: %w", err)
	}

	emails, err := services.LoadEmailCatalog()
	if err != nil {
		return err
	}
	cfg.Mail.From = emails.From
	mailer, err := services.NewMailer(cfg.Mail)
	if err != nil {
		return err
	}

	authService := services.NewAuthService()
	h := handlers.NewHandler(db, authService, storage, mailer, emails, handlers.Settings{
		Version:      version,
		Mode:         cfg.Mode,
		StorageQuota: cfg.StorageQuota,
		ServerSecret: serverSecret(cfg.ServerSecret),
	})

	router, err := NewRouter(h, db, authService, cfg.TrustedProxies)
	if err != nil {
		return err
	}
	pruneCtx, stopPrune := context.WithCancel(context.Background())
	defer stopPrune()
	go handlers.PruneEmailCodesEvery(pruneCtx, db, time.Hour)
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

// Migrate applies every pending migration; the server calls it before accepting requests.
func Migrate(db *sql.DB, dir string) error {
	p, err := newMigrator(db, dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	results, err := p.Up(ctx)
	for _, r := range withPartial(results, err) {
		if r.Error != nil {
			log.Printf("migration %s failed: %v", r.Source.Path, r.Error)
		} else {
			log.Printf("migration %s applied", r.Source.Path)
		}
	}
	return err
}
