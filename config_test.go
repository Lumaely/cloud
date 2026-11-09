package main

import (
	"cloud/handlers"
	"cloud/services"
	"reflect"
	"testing"
)

var configEnvKeys = []string{
	"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
	"SERVER_MODE", "SERVER_SECRET", "STORAGE_QUOTA_BYTES", "MAIL_DRIVER", "MAIL_FROM", "TRUSTED_PROXIES",
	"MINIO_ENDPOINT", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "MINIO_BUCKET", "MINIO_REGION", "MINIO_USE_SSL",
}

func clearConfigEnv(t *testing.T) {
	for _, k := range configEnvKeys {
		t.Setenv(k, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := loadConfig()

	want := Config{
		DBHost: "localhost", DBPort: "5432", DBUser: "Admin",
		DBPassword: "LumaelyPasswordChangeMePlz", DBName: "Lumaely-db",
		Mode: handlers.ModeCloud, StorageQuota: 5 << 30,
		MailDriver: "log", MailFrom: "Lumaely <no-reply@lumaely.app>",
		S3: services.S3Config{
			Endpoint: "localhost:9000", AccessKey: "minioadmin", SecretKey: "minioadmin",
			Bucket: "journal-attachments", Region: "us-east-1", UseSSL: false,
		},
	}
	if err != nil || !reflect.DeepEqual(cfg, want) {
		t.Errorf("loadConfig() = %+v, %v\nwant %+v", cfg, err, want)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DB_HOST", "db")
	t.Setenv("SERVER_MODE", handlers.ModeSelfHosted)
	t.Setenv("SERVER_SECRET", "s")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.1, 10.0.0.0/8 ,")
	t.Setenv("MINIO_ENDPOINT", "s3:9000")
	t.Setenv("MINIO_USE_SSL", "true")

	cfg, err := loadConfig()

	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBHost != "db" || cfg.Mode != handlers.ModeSelfHosted || cfg.StorageQuota != 0 || cfg.ServerSecret != "s" {
		t.Errorf("config not read from env: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.TrustedProxies, []string{"10.0.0.1", "10.0.0.0/8"}) {
		t.Errorf("TrustedProxies = %v", cfg.TrustedProxies)
	}
	if cfg.S3.Endpoint != "s3:9000" || !cfg.S3.UseSSL {
		t.Errorf("s3 config not read from env: %+v", cfg.S3)
	}
}

func TestLoadConfigQuotaOverride(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("STORAGE_QUOTA_BYTES", "1024")

	cfg, err := loadConfig()

	if err != nil || cfg.StorageQuota != 1024 {
		t.Errorf("quota = %d, %v", cfg.StorageQuota, err)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for key, value := range map[string]string{
		"SERVER_MODE":         "hybrid",
		"STORAGE_QUOTA_BYTES": "-1",
	} {
		clearConfigEnv(t)
		t.Setenv(key, value)
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s=%s accepted", key, value)
		}
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

func TestRunCommands(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Errorf("version: %v", err)
	}
	if err := run([]string{"pair"}); err == nil {
		t.Error("pair should report it is not available yet")
	}
	if err := run([]string{"nope"}); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestServerSecret(t *testing.T) {
	if string(serverSecret("abc")) != "abc" {
		t.Error("configured secret not used")
	}
	if a, b := serverSecret(""), serverSecret(""); len(a) != 32 || string(a) == string(b) {
		t.Error("fallback secret is not random")
	}
}
