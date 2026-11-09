package main

import (
	"cloud/services"
	"testing"
)

var configEnvKeys = []string{
	"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
	"MINIO_ENDPOINT", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "MINIO_BUCKET", "MINIO_REGION", "MINIO_USE_SSL",
}

func clearConfigEnv(t *testing.T) {
	for _, k := range configEnvKeys {
		t.Setenv(k, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg := loadConfig()

	want := Config{
		DBHost: "localhost", DBPort: "5432", DBUser: "Admin",
		DBPassword: "LumaelyPasswordChangeMePlz", DBName: "Lumaely-db",
		S3: services.S3Config{
			Endpoint: "localhost:9000", AccessKey: "minioadmin", SecretKey: "minioadmin",
			Bucket: "journal-attachments", Region: "us-east-1", UseSSL: false,
		},
	}
	if cfg != want {
		t.Errorf("loadConfig() = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DB_HOST", "db")
	t.Setenv("DB_NAME", "app")
	t.Setenv("MINIO_ENDPOINT", "s3:9000")
	t.Setenv("MINIO_BUCKET", "files")
	t.Setenv("MINIO_REGION", "eu-west-3")
	t.Setenv("MINIO_USE_SSL", "true")

	cfg := loadConfig()

	if cfg.DBHost != "db" || cfg.DBName != "app" {
		t.Errorf("db config not read from env: %+v", cfg)
	}
	if cfg.S3.Endpoint != "s3:9000" || cfg.S3.Bucket != "files" || cfg.S3.Region != "eu-west-3" || !cfg.S3.UseSSL {
		t.Errorf("s3 config not read from env: %+v", cfg.S3)
	}
}

func TestPostgresDSN(t *testing.T) {
	cfg := Config{DBHost: "h", DBPort: "1", DBUser: "u", DBPassword: "p", DBName: "n"}
	const want = "host=h port=1 user=u password=p dbname=n sslmode=disable"
	if got := cfg.PostgresDSN(); got != want {
		t.Errorf("PostgresDSN() = %q, want %q", got, want)
	}
}

func TestGetEnv(t *testing.T) {
	t.Setenv("CLOUD_TEST_VAR", "")
	if got := getEnv("CLOUD_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("empty var: got %q", got)
	}
	t.Setenv("CLOUD_TEST_VAR", "set")
	if got := getEnv("CLOUD_TEST_VAR", "fallback"); got != "set" {
		t.Errorf("set var: got %q", got)
	}
}

func TestGetEnvBool(t *testing.T) {
	cases := []struct {
		value    string
		fallback bool
		want     bool
	}{
		{"", true, true},
		{"", false, false},
		{"true", false, true},
		{"1", false, true},
		{"false", true, false},
		{"not-a-bool", true, true},
	}
	for _, tc := range cases {
		t.Setenv("CLOUD_TEST_BOOL", tc.value)
		if got := getEnvBool("CLOUD_TEST_BOOL", tc.fallback); got != tc.want {
			t.Errorf("getEnvBool(%q, %v) = %v, want %v", tc.value, tc.fallback, got, tc.want)
		}
	}
}
