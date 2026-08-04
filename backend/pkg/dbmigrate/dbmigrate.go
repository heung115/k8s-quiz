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

// UpIsolatedTestSchema installs the provider-neutral domain schema through
// version 14 in a non-public test schema. Version 15 is deliberately excluded:
// it pins the deployment authority function to public and is exercised only by
// dedicated-database strict-role tests.
func UpIsolatedTestSchema(databaseURL string) error {
	return migrateTo(migrations.FS, ".", databaseURL, 14)
}

// UpFS is the testable core of Up: run migrations from any fs.FS whose root
// (dir) holds the N_name.up.sql / N_name.down.sql files.
func UpFS(fsys fs.FS, dir, databaseURL string) error {
	return migrateTo(fsys, dir, databaseURL, 0)
}

func migrateTo(fsys fs.FS, dir, databaseURL string, target uint) error {
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

	if target == 0 {
		return upFailClosed(m)
	}
	if err := m.Migrate(target); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate to version %d: %w", target, err)
	}
	return logVersion(m)
}

// upFailClosed never changes migration bookkeeping after a failed or
// interrupted migration. Automatically forcing an arbitrary dirty version
// clean can mark a partially-applied schema complete, especially once a
// migration spans multiple related Runner tables and constraints. Operators
// must inspect and repair the exact migration before explicitly forcing it.
func upFailClosed(m *migrate.Migrate) error {
	err := m.Up()
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return logVersion(m)
	}
	var dirty migrate.ErrDirty
	if errors.As(err, &dirty) {
		return fmt.Errorf("migrate up: database is dirty at version %d; inspect and repair the migration before explicitly forcing a version: %w", dirty.Version, err)
	}
	return fmt.Errorf("migrate up: %w", err)
}

func logVersion(m *migrate.Migrate) error {
	if v, dirty, verr := m.Version(); verr == nil {
		log.Printf("database schema at version %d (dirty=%v)", v, dirty)
	}
	return nil
}
