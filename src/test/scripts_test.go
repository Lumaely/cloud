package test

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationFixtures are three small migrations, so a target can sit between applied and pending ones.
var migrationFixtures = map[string]string{
	"00001_create_alpha.sql": "-- +goose Up\nCREATE TABLE alpha (id INT);\n-- +goose Down\nDROP TABLE alpha;\n",
	"00002_create_beta.sql":  "-- +goose Up\nCREATE TABLE beta (id INT);\n-- +goose Down\nDROP TABLE beta;\n",
	"00003_create_gamma.sql": "-- +goose Up\nCREATE TABLE gamma (id INT);\n-- +goose Down\nDROP TABLE gamma;\n",
}

type scriptEnv struct {
	t   *testing.T
	db  *sql.DB
	dir string
	dsn string
}

// newScriptEnv needs TEST_DATABASE_URL; it recreates the public schema of that database.
func newScriptEnv(t *testing.T, fixtures map[string]string) *scriptEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &scriptEnv{t: t, db: db, dir: dir, dsn: dsn}
}

func (e *scriptEnv) run(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("../scripts/apply_migration", append([]string{"--local"}, args...)...)
	// Empty MIGRATIONS_DIR selects the embedded migrations.
	cmd.Env = append(os.Environ(), "DATABASE_URL="+e.dsn, "MIGRATIONS_DIR="+e.dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *scriptEnv) tables() []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN ('alpha', 'beta', 'gamma', 'delta') ORDER BY table_name`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func assertTables(t *testing.T, e *scriptEnv, want string) {
	t.Helper()
	if got := strings.Join(e.tables(), ","); got != want {
		t.Errorf("tables = %q, want %q", got, want)
	}
}

func TestApplyMigrationScriptTargetsOneFile(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)

	out, err := e.run("00002_create_beta.sql")

	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	assertTables(t, e, "alpha,beta")
}

func TestApplyMigrationScriptAcceptsVersionAndPath(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)

	if out, err := e.run("1"); err != nil {
		t.Fatalf("version target: %v\n%s", err, out)
	}
	assertTables(t, e, "alpha")

	if out, err := e.run(filepath.Join(e.dir, "00003_create_gamma.sql")); err != nil {
		t.Fatalf("path target: %v\n%s", err, out)
	}
	assertTables(t, e, "alpha,beta,gamma")
}

func TestApplyMigrationScriptAppliesAllPendingWithoutTarget(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)

	out, err := e.run()

	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	assertTables(t, e, "alpha,beta,gamma")
}

func TestApplyMigrationScriptRejectsAppliedTarget(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)
	if out, err := e.run("00002_create_beta.sql"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}

	out, err := e.run("00001_create_alpha.sql")

	if err == nil {
		t.Fatalf("applied target accepted:\n%s", out)
	}
	if !strings.Contains(out, "already applied") {
		t.Errorf("output does not explain the refusal:\n%s", out)
	}
	assertTables(t, e, "alpha,beta")
}

func TestApplyMigrationScriptRejectsUnknownTarget(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)

	out, err := e.run("00009_missing.sql")

	if err == nil {
		t.Fatalf("unknown target accepted:\n%s", out)
	}
	if !strings.Contains(out, "unknown migration") {
		t.Errorf("output does not name the problem:\n%s", out)
	}
	assertTables(t, e, "")
}

func TestApplyMigrationScriptRollsBackFailingMigration(t *testing.T) {
	broken := map[string]string{
		"00001_create_alpha.sql": migrationFixtures["00001_create_alpha.sql"],
		"00002_broken.sql":       "-- +goose Up\nCREATE TABLE delta (id INT);\nSELECT * FROM missing_table;\n-- +goose Down\nDROP TABLE delta;\n",
	}
	e := newScriptEnv(t, broken)

	out, err := e.run()

	if err == nil {
		t.Fatalf("failing migration reported success:\n%s", out)
	}
	for _, want := range []string{"applied  00001_create_alpha.sql", "failed   00002_broken.sql"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no migration to apply") {
		t.Errorf("output claims nothing ran:\n%s", out)
	}
	assertTables(t, e, "alpha")
}

func TestApplyMigrationScriptStatus(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)
	if out, err := e.run("1"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}

	out, err := e.run("--status")

	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, out)
	}
	for _, line := range []string{"00001_create_alpha.sql", "00003_create_gamma.sql"} {
		if !strings.Contains(out, line) {
			t.Errorf("status misses %s:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "Pending") {
		t.Errorf("status does not show pending migrations:\n%s", out)
	}
	assertTables(t, e, "alpha")
}

func TestApplyMigrationScriptAppliesEmbeddedBaseline(t *testing.T) {
	e := newScriptEnv(t, nil)
	e.dir = ""

	out, err := e.run()
	if err != nil {
		t.Fatalf("baseline failed: %v\n%s", err, out)
	}
	var tables int
	err = e.db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN
		('users', 'user_sessions', 'user_challenges', 'mood_entries', 'entry_attachments', 'patterns',
		 'activities', 'places', 'bookmark_folders', 'bookmarks', 'feedback')`).Scan(&tables)
	if err != nil {
		t.Fatal(err)
	}
	if tables != 11 {
		t.Errorf("baseline created %d of 11 tables", tables)
	}

	out, err = e.run()
	if err != nil || !strings.Contains(out, "no migration to apply") {
		t.Errorf("second run should be a no-op: %v\n%s", err, out)
	}
}

func TestApplyMigrationScriptRejectsStatusWithTarget(t *testing.T) {
	e := newScriptEnv(t, migrationFixtures)

	for _, args := range [][]string{{"--status", "2"}, {"2", "--status"}} {
		out, err := e.run(args...)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("%v: err = %v, want exit code 2\n%s", args, err, out)
		}
	}
	assertTables(t, e, "")
}
