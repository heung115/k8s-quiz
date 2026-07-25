// Package dbmigrate applies the embedded SQL migrations at boot (INFRA-4),
// so the server never runs against an un-migrated schema and no separate
// migrate CLI step or filesystem mount is required.
package dbmigrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" database/sql driver

	"github.com/k8s-quiz/backend/migrations"
)

// Up applies all pending UP migrations from the embedded migrations.FS
// against databaseURL. It is a no-op when the schema is already current and
// logs the resulting schema version.
func Up(databaseURL string) error {
	return UpFS(migrations.FS, ".", databaseURL)
}

// UpFS is the testable core of Up: run migrations from any fs.FS whose root
// (dir) holds the N_name.up.sql / N_name.down.sql files.
func UpFS(fsys fs.FS, dir, databaseURL string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()

	drv, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx", drv)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}

	return upWithDirtyHeal(m)
}

// upWithDirtyHeal runs Up and self-heals a dirty schema_migrations row.
// Legacy volumes were seeded by mounting migrations straight into
// docker-entrypoint-initdb.d: they have the full 001–003 schema but no
// bookkeeping, so the first run replayed 001 ("relation already exists") and
// left schema_migrations dirty. Because every UP migration is idempotent DDL,
// we can safely force the dirty version clean and retry once — this converges
// any legacy volume to the latest version.
func upWithDirtyHeal(m *migrate.Migrate) error {
	err := m.Up()
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return logVersion(m)
	}

	v, ok := dirtyVersion(err)
	if !ok {
		return fmt.Errorf("migrate up: %w", err)
	}

	log.Printf("WARNING: database marked dirty at version %d (legacy volume seeded before migration bookkeeping existed); forcing version %d clean and retrying with idempotent DDL", v, v)
	if ferr := m.Force(v); ferr != nil {
		return fmt.Errorf("migrate up: %w (force version %d failed: %v)", err, v, ferr)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up after dirty recovery: %w", err)
	}
	return logVersion(m)
}

func logVersion(m *migrate.Migrate) error {
	if v, dirty, verr := m.Version(); verr == nil {
		log.Printf("database schema at version %d (dirty=%v)", v, dirty)
	}
	return nil
}

// dirtyVersion extracts the schema version from a dirty-database error
// (migrate.ErrDirty, with a message-parse fallback).
func dirtyVersion(err error) (int, bool) {
	var de migrate.ErrDirty
	if errors.As(err, &de) {
		return de.Version, true
	}
	if msg := err.Error(); strings.Contains(msg, "Dirty database") {
		var v int
		if _, serr := fmt.Sscanf(msg, "Dirty database version %d", &v); serr == nil {
			return v, true
		}
	}
	return 0, false
}
