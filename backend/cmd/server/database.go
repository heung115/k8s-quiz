package main

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/dbsecurity"
)

func runtimeDatabasePoolConfig(cfg *config.Config) (*pgxpool.Config, error) {
	if cfg == nil {
		return nil, errors.New("database config is required")
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if !cfg.DatabaseSecurityConfigured() {
		return poolConfig, nil
	}
	if err := dbsecurity.ConfigureRuntimePool(poolConfig, databaseSecurityConfig(cfg)); err != nil {
		return nil, fmt.Errorf("configure least-privilege runtime pool: %w", err)
	}
	return poolConfig, nil
}

func databaseSecurityConfig(cfg *config.Config) dbsecurity.Config {
	if cfg == nil {
		return dbsecurity.Config{}
	}
	return dbsecurity.Config{
		CatalogOwnerRole: cfg.DatabaseCatalogOwnerRole,
		OwnerRole:        cfg.DatabaseOwnerRole,
		MigratorRole:     cfg.DatabaseMigratorRole,
		RuntimeRole:      cfg.DatabaseRuntimeRole,
		ValidatorRole:    cfg.DatabaseValidatorRole,
		DedicatedCluster: cfg.DatabaseDedicatedCluster,
	}
}
