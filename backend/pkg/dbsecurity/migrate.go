package dbsecurity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Migrate applies embedded migrations through a migrator login that can only
// SET ROLE to the NOLOGIN application owner. Every migration therefore creates
// public-schema objects with the durable owner identity.
func Migrate(ctx context.Context, fsys fs.FS, dir, databaseURL, ownerRole string) error {
	if fsys == nil || databaseURL == "" || dir == "" {
		return ErrInvalidConfig
	}
	quotedOwner, err := quoteIdentifier(ownerRole)
	if err != nil {
		return err
	}
	connConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return errors.New("parse migrator database URL")
	}
	connConfig.RuntimeParams["search_path"] = "public, pg_catalog"
	sqlDB := stdlib.OpenDB(*connConfig, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET ROLE "+quotedOwner); err != nil {
			return errors.New("set Control Plane database owner role")
		}
		var current, session, schema string
		if err := conn.QueryRow(ctx, `SELECT current_user,session_user,current_schema()`).Scan(
			&current, &session, &schema); err != nil {
			return errors.New("verify migrator database identity")
		}
		if current != ownerRole || current == session || schema != Schema {
			return errors.New("migrator database identity is unsafe")
		}
		return nil
	}))
	defer sqlDB.Close()
	if err := pingContext(ctx, sqlDB); err != nil {
		return err
	}
	source, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("open embedded migration source: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("open PostgreSQL migration driver: %w", err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "pgx", driver)
	if err != nil {
		return fmt.Errorf("initialize Control Plane migrations: %w", err)
	}
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return fmt.Errorf("Control Plane database is dirty at migration %d; inspect and repair it: %w", dirty.Version, err)
		}
		return fmt.Errorf("apply Control Plane migrations: %w", err)
	}
	return nil
}

func pingContext(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return errors.New("connect to migrator database")
	}
	return nil
}
