package main

import (
	"cloud/db/migrations"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// newMigrator reads the embedded migrations, or dir when set (MIGRATIONS_DIR).
// The Postgres session lock lets only one process migrate at a time.
func newMigrator(db *sql.DB, dir string) (*goose.Provider, error) {
	var fsys fs.FS = migrations.FS
	if dir != "" {
		fsys = os.DirFS(dir)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
}

func migrateCommand(ctx context.Context, args []string, out io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := newMigrator(db, cfg.MigrationsDir)
	if err != nil {
		return err
	}

	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "up"):
		results, err := p.Up(ctx)
		printResults(out, results)
		return err
	case len(args) == 2 && args[0] == "up-to":
		return migrateUpTo(ctx, p, args[1], out)
	case len(args) == 1 && args[0] == "status":
		return printStatus(ctx, p, out)
	default:
		return errors.New("usage: migrate [up | up-to TARGET | status]")
	}
}

// migrateUpTo applies pending migrations up to and including target, a file name, path or version.
func migrateUpTo(ctx context.Context, p *goose.Provider, target string, out io.Writer) error {
	src, err := resolveTarget(p.ListSources(), target)
	if err != nil {
		return err
	}
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	if src.Version <= current {
		return fmt.Errorf("migration %s is already applied (database is at version %d)", filepath.Base(src.Path), current)
	}
	results, err := p.UpTo(ctx, src.Version)
	printResults(out, results)
	return err
}

func resolveTarget(sources []*goose.Source, target string) (*goose.Source, error) {
	name := filepath.Base(target)
	version, numErr := strconv.ParseInt(name, 10, 64)
	for _, s := range sources {
		if (numErr == nil && s.Version == version) || filepath.Base(s.Path) == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("unknown migration %q", target)
}

func printResults(out io.Writer, results []*goose.MigrationResult) {
	if len(results) == 0 {
		fmt.Fprintln(out, "no migration to apply")
	}
	for _, r := range results {
		state := "applied"
		if r.Error != nil {
			state = "failed"
		}
		fmt.Fprintf(out, "%-8s %s (%s)\n", state, filepath.Base(r.Source.Path), r.Duration.Round(1e6))
	}
}

func printStatus(ctx context.Context, p *goose.Provider, out io.Writer) error {
	statuses, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		if s.State == goose.StateApplied {
			fmt.Fprintf(out, "Applied  %s  %s\n", s.AppliedAt.Format("2006-01-02 15:04:05"), filepath.Base(s.Source.Path))
		} else {
			fmt.Fprintf(out, "Pending  %-19s  %s\n", "-", filepath.Base(s.Source.Path))
		}
	}
	return nil
}
