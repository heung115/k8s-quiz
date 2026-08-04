package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/migrations"
	"github.com/k8s-quiz/backend/pkg/dbsecurity"
)

const (
	commandTimeout = 2 * time.Minute
	maxSecretBytes = 64 * 1024
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "control-plane-db:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("expected migrate, harden, or check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	switch arguments[0] {
	case "migrate":
		return runMigrate(ctx, arguments[1:], stdout, stderr)
	case "harden":
		return runHarden(ctx, arguments[1:], stdout, stderr)
	case "check":
		return runCheck(ctx, arguments[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runMigrate(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databaseURLFile := flags.String("database-url-file", "", "mode-0600 migrator database URL file")
	ownerRole := flags.String("owner-role", "", "NOLOGIN public-schema owner role")
	if err := flags.Parse(arguments); err != nil {
		return errors.New("invalid migrate arguments")
	}
	if flags.NArg() != 0 || *ownerRole == "" {
		return errors.New("migrate requires --database-url-file and --owner-role")
	}
	databaseURL, err := loadSecretFile(*databaseURLFile)
	if err != nil {
		return err
	}
	if err := dbsecurity.Migrate(ctx, migrations.FS, ".", databaseURL, *ownerRole); err != nil {
		return fmt.Errorf("migrate Control Plane database: %w", redactDatabaseError(err))
	}
	_, _ = fmt.Fprintln(stdout, "Control Plane database migration complete")
	return nil
}

func runHarden(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("harden", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databaseURLFile := flags.String("database-url-file", "", "mode-0600 provisioning database URL file")
	config := roleFlags(flags)
	dedicated := flags.Bool("dedicated-postgres-cluster", false, "confirm the whole PostgreSQL cluster is dedicated to the Control Plane")
	if err := flags.Parse(arguments); err != nil {
		return errors.New("invalid harden arguments")
	}
	config.DedicatedCluster = *dedicated
	if flags.NArg() != 0 || !rolesPresent(config) {
		return errors.New("harden requires a database URL file and all Control Plane role names")
	}
	pool, err := openPool(ctx, *databaseURLFile, nil)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := dbsecurity.Harden(ctx, pool, config); err != nil {
		return fmt.Errorf("harden Control Plane database: %w", redactDatabaseError(err))
	}
	_, _ = fmt.Fprintln(stdout, "Control Plane database hardening complete")
	return nil
}

func runCheck(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeURLFile := flags.String("runtime-database-url-file", "", "mode-0600 runtime database URL file")
	validatorURLFile := flags.String("validator-database-url-file", "", "mode-0600 proof validator database URL file")
	config := roleFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return errors.New("invalid check arguments")
	}
	config.DedicatedCluster = true
	if flags.NArg() != 0 || !rolesPresent(config) || *runtimeURLFile == "" || *validatorURLFile == "" {
		return errors.New("check requires private runtime/validator URL files and all Control Plane role names")
	}
	runtime, err := openPool(ctx, *runtimeURLFile, func(poolConfig *pgxpool.Config) error {
		return dbsecurity.ConfigureRuntimePool(poolConfig, config)
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
	validator, err := openPool(ctx, *validatorURLFile, func(poolConfig *pgxpool.Config) error {
		return dbsecurity.ConfigureValidatorPool(poolConfig, config)
	})
	if err != nil {
		return err
	}
	defer validator.Close()
	if err := dbsecurity.Verify(ctx, runtime, validator, config); err != nil {
		return fmt.Errorf("verify Control Plane database security: %w", redactDatabaseError(err))
	}
	_, _ = fmt.Fprintln(stdout, "Control Plane database security verified")
	return nil
}

func roleFlags(flags *flag.FlagSet) dbsecurity.Config {
	config := dbsecurity.Config{}
	flags.StringVar(&config.CatalogOwnerRole, "catalog-owner-role", "", "trusted owner of pg_catalog built-ins")
	flags.StringVar(&config.OwnerRole, "owner-role", "", "NOLOGIN public-schema owner")
	flags.StringVar(&config.MigratorRole, "migrator-role", "", "migration login")
	flags.StringVar(&config.RuntimeRole, "runtime-role", "", "Control Plane runtime login")
	flags.StringVar(&config.ValidatorRole, "validator-role", "", "proof validator login")
	return config
}

func rolesPresent(config dbsecurity.Config) bool {
	return config.CatalogOwnerRole != "" && config.OwnerRole != "" && config.MigratorRole != "" &&
		config.RuntimeRole != "" && config.ValidatorRole != ""
}

func openPool(ctx context.Context, path string, configure func(*pgxpool.Config) error) (*pgxpool.Pool, error) {
	databaseURL, err := loadSecretFile(path)
	if err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("parse database URL file")
	}
	if configure != nil {
		if err := configure(poolConfig); err != nil {
			return nil, errors.New("configure database connection security")
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, errors.New("open database connection")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("connect to database")
	}
	return pool, nil
}

func loadSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("database URL file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("inspect database URL file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("database URL file must be regular and accessible only by its owner")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open database URL file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return "", errors.New("database URL file changed or is not private")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("database URL file must be owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil {
		return "", errors.New("read database URL file")
	}
	if len(data) == 0 || len(data) > maxSecretBytes {
		return "", errors.New("database URL file is empty or too large")
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("database URL file must contain exactly one value")
	}
	return value, nil
}

func redactDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New("database operation failed; inspect PostgreSQL logs and role configuration")
}
