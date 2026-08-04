package dbmigrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/migrations"
)

var migrationFilePattern = regexp.MustCompile(`^0*([1-9][0-9]*)_.+\.up\.sql$`)

// RequireCurrent is a read-only public startup gate. Public server processes
// never apply schema changes; they only prove the one-shot migrator completed.
func RequireCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database pool is required")
	}
	want, err := latestVersion(migrations.FS, ".")
	if err != nil {
		return err
	}
	var got int
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version,dirty FROM public.schema_migrations LIMIT 1`).Scan(&got, &dirty); err != nil {
		return fmt.Errorf("read public.schema_migrations: %w", err)
	}
	if dirty || got != want {
		return fmt.Errorf("database migration state is version %d dirty=%t; expected version %d dirty=false", got, dirty, want)
	}
	return nil
}

func latestVersion(fsys fs.FS, dir string) (int, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	latest := 0
	for _, entry := range entries {
		match := migrationFilePattern.FindStringSubmatch(entry.Name())
		if entry.IsDir() || match == nil || strings.Contains(entry.Name(), "..") {
			continue
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, fmt.Errorf("parse migration version %q: %w", match[1], err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, errors.New("embedded migrations contain no up migration")
	}
	return latest, nil
}
