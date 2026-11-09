package main

import (
	"cloud/services"
	"fmt"
	"os"
	"strconv"
)

// Config is the runtime configuration read from the environment.
type Config struct {
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	S3         services.S3Config
}

func loadConfig() Config {
	return Config{
		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "5432"),
		DBUser:     getEnv("DB_USER", "Admin"),
		DBPassword: getEnv("DB_PASSWORD", "LumaelyPasswordChangeMePlz"),
		DBName:     getEnv("DB_NAME", "Lumaely-db"),
		S3: services.S3Config{
			Endpoint:  getEnv("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey: getEnv("MINIO_ROOT_USER", "minioadmin"),
			SecretKey: getEnv("MINIO_ROOT_PASSWORD", "minioadmin"),
			Bucket:    getEnv("MINIO_BUCKET", "journal-attachments"),
			Region:    getEnv("MINIO_REGION", "us-east-1"),
			UseSSL:    getEnvBool("MINIO_USE_SSL", false),
		},
	}
}

func (c Config) PostgresDSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	b, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return b
}
