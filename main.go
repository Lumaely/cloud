package main

import (
	"cloud/handlers"
	"cloud/services"
	"context"
	"database/sql"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	cfg := loadConfig()

	db, err := sql.Open("pgx", cfg.PostgresDSN())
	if err != nil {
		log.Fatalf("Database connection configuration error: %v\n", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	storage, err := services.NewS3Storage(ctx, cfg.S3)
	cancel()
	if err != nil {
		log.Fatalf("Object storage init error: %v\n", err)
	}

	authService := services.NewAuthService()
	h := handlers.NewHandler(db, &services.MockStripeService{}, authService, storage)

	if err := newRouter(h, db, authService).Run(":8080"); err != nil {
		log.Fatalf("Server error: %v\n", err)
	}
}
