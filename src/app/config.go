package app

import (
	"cloud/src/handlers"
	"cloud/src/services"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const defaultQuotaBytes = 5 << 30

// Config is the runtime configuration read from the environment.
type Config struct {
	DatabaseURL    string
	MigrationsDir  string
	DBHost         string
	DBPort         string
	DBUser         string
	DBPassword     string
	DBName         string
	Mode           string
	ServerSecret   string
	StorageQuota   int64
	Mail           services.MailConfig
	TrustedProxies []string
	S3             services.S3Config
}

func loadConfig() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		MigrationsDir: os.Getenv("MIGRATIONS_DIR"),
		DBHost:        getEnv("DB_HOST", "localhost"),
		DBPort:        getEnv("DB_PORT", "5432"),
		DBUser:        getEnv("DB_USER", "Admin"),
		DBPassword:    getEnv("DB_PASSWORD", "LumaelyPasswordChangeMePlz"),
		DBName:        getEnv("DB_NAME", "Lumaely-db"),
		Mode:          getEnv("SERVER_MODE", handlers.ModeCloud),
		ServerSecret:  os.Getenv("SERVER_SECRET"),
		Mail: services.MailConfig{
			Driver:    getEnv("MAIL_DRIVER", "log"),
			From:      getEnv("MAIL_FROM", "Lumaely <no-reply@lumaely.app>"),
			SecretKey: os.Getenv("SCW_SECRET_KEY"),
			ProjectID: os.Getenv("SCW_PROJECT_ID"),
			Region:    getEnv("SCW_REGION", "fr-par"),
		},
		TrustedProxies: splitList(os.Getenv("TRUSTED_PROXIES")),
		S3: services.S3Config{
			Endpoint:  getEnv("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey: getEnv("MINIO_ROOT_USER", "minioadmin"),
			SecretKey: getEnv("MINIO_ROOT_PASSWORD", "minioadmin"),
			Bucket:    getEnv("MINIO_BUCKET", "journal-attachments"),
			Region:    getEnv("MINIO_REGION", "us-east-1"),
			UseSSL:    getEnvBool("MINIO_USE_SSL", false),
		},
	}

	switch cfg.Mode {
	case handlers.ModeCloud:
		cfg.StorageQuota = defaultQuotaBytes
	case handlers.ModeSelfHosted:
	default:
		return cfg, fmt.Errorf("SERVER_MODE must be %q or %q", handlers.ModeCloud, handlers.ModeSelfHosted)
	}
	if v := os.Getenv("STORAGE_QUOTA_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("STORAGE_QUOTA_BYTES must be a non-negative integer")
		}
		cfg.StorageQuota = n
	}
	return cfg, nil
}

// PostgresDSN prefers DATABASE_URL over the DB_* parts.
func (c Config) PostgresDSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
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

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
