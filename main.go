package main

import (
	"cloud/handlers"
	"cloud/middleware"
	"cloud/services"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5432"
	}
	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "Admin"
	}
	dbPassword := os.Getenv("DB_PASSWORD")
	if dbPassword == "" {
		dbPassword = "LumaelyPasswordChangeMePlz"
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "Lumaely-db"
	}

	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName)

	db, err := sql.Open("pgx", psqlInfo)
	if err != nil {
		log.Fatalf("Database connection configuration error: %v\n", err)
	}
	defer db.Close()

	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	if minioEndpoint == "" {
		minioEndpoint = "localhost:9000"
	}
	minioAccessKey := os.Getenv("MINIO_ROOT_USER")
	if minioAccessKey == "" {
		minioAccessKey = "minioadmin"
	}
	minioSecretKey := os.Getenv("MINIO_ROOT_PASSWORD")
	if minioSecretKey == "" {
		minioSecretKey = "minioadmin"
	}
	minioBucket := os.Getenv("MINIO_BUCKET")
	if minioBucket == "" {
		minioBucket = "journal-attachments"
	}

	storageService, err := services.NewMinIOStorage(minioEndpoint, minioAccessKey, minioSecretKey, minioBucket, false)
	if err != nil {
		log.Printf("MinIO connection warning: %v\n", err)
	}

	mockStripe := &services.MockStripeService{}
	authService := services.NewAuthService()
	h := handlers.NewHandler(db, mockStripe, authService, storageService)

	r := gin.Default()

	r.GET("/health", h.HealthCheck)
	r.POST("/users/register", h.RegisterUser)
	r.POST("/users/login", h.Login)

	protected := r.Group("")
	protected.Use(middleware.AuthRequired(db, authService))
	{
		protected.GET("/users/me", h.GetUserProfile)
		protected.DELETE("/users/me", h.DeleteUserProfile)
		protected.GET("/sync", h.FetchDelta)
		protected.POST("/sync", h.PushChanges)
	}

	r.Run(":8080")
}
