package dbmigrate

import (
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/stub"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	controlplanedb "github.com/heung115/k8s-quiz/runnerprotocol/controlplanedb"

	"github.com/k8s-quiz/backend/migrations"
)

const latestMigrationVersion = 17

// INFRA-4: the embedded FS must contain every migration pair and the
// migrate source must parse the versions (no DB needed).
func TestEmbeddedMigrationsParse(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, want := range []string{
		"001_init.up.sql", "001_init.down.sql",
		"004_refresh_token_families.up.sql", "004_refresh_token_families.down.sql",
		"005_add_problem_revision.up.sql", "005_add_problem_revision.down.sql",
		"006_add_durable_runner_state.up.sql", "006_add_durable_runner_state.down.sql",
		"007_add_runner_controller_epochs.up.sql", "007_add_runner_controller_epochs.down.sql",
		"008_add_lifecycle_event_keys.up.sql", "008_add_lifecycle_event_keys.down.sql",
		"009_guard_running_verify_per_allocation.up.sql", "009_guard_running_verify_per_allocation.down.sql",
		"010_add_problem_catalog_active.up.sql", "010_add_problem_catalog_active.down.sql",
		"011_add_problem_catalog_ledger.up.sql", "011_add_problem_catalog_ledger.down.sql",
		"012_add_problem_artifact_bindings.up.sql", "012_add_problem_artifact_bindings.down.sql",
		"013_add_durable_end_operation.up.sql", "013_add_durable_end_operation.down.sql",
		"014_add_runner_controller_proofs.up.sql", "014_add_runner_controller_proofs.down.sql",
		"015_pin_controller_proof_schema.up.sql", "015_pin_controller_proof_schema.down.sql",
		"016_recheck_controller_proof_after_lock.up.sql", "016_recheck_controller_proof_after_lock.down.sql",
		"017_version_controller_proof_consumer.up.sql", "017_version_controller_proof_consumer.down.sql",
	} {
		if !names[want] {
			t.Errorf("embedded migrations missing %s (have %v)", want, names)
		}
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	first, err := src.First()
	if err != nil {
		t.Fatalf("first version: %v", err)
	}
	if first != 1 {
		t.Errorf("expected first version 1, got %d", first)
	}
	// Walk to the latest version.
	v := first
	for {
		next, err := src.Next(v)
		if err != nil {
			break
		}
		v = next
	}
	if v != latestMigrationVersion {
		t.Errorf("expected latest version %d, got %d", latestMigrationVersion, v)
	}
}

func TestMigration017ProofConsumerBodyMatchesSharedSecurityContract(t *testing.T) {
	source, err := migrations.FS.ReadFile("017_version_controller_proof_consumer.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?s)CREATE FUNCTION public\.consume_runner_controller_proof_v2\(.*?AS \$function\$(.*?)\$function\$;`)
	match := pattern.FindSubmatch(source)
	if len(match) != 2 {
		t.Fatal("migration 017 v2 proof consumer body is absent")
	}
	if !controlplanedb.ProofConsumerBodyMatches(string(match[1])) {
		t.Fatal("migration 017 v2 proof consumer body drifted from the shared security contract")
	}
	if strings.Contains(string(source), "CREATE FUNCTION public.consume_runner_controller_proof(") {
		t.Fatal("migration 017 recreates the legacy proof consumer")
	}
}

// --- dirty-database fail-closed behavior ---

func newTestMigrate(t *testing.T, drv database.Driver) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "stub", drv)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	return m
}

// A dirty bookkeeping row may represent a partially-applied migration. Startup
// must not force it clean or run later migrations without operator inspection.
func TestUpRejectsDirtyDatabaseWithoutForce(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: 1, IsDirty: true, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	err := upFailClosed(m)
	if err == nil || !strings.Contains(err.Error(), "dirty at version 1") {
		t.Fatalf("dirty database error = %v", err)
	}
	if drv.CurrentVersion != 1 || !drv.IsDirty {
		t.Errorf("startup changed dirty bookkeeping: version=%d dirty=%v", drv.CurrentVersion, drv.IsDirty)
	}
	if len(drv.MigrationSequence) != 0 {
		t.Errorf("startup ran %d migrations after dirty version", len(drv.MigrationSequence))
	}
}

// A fresh volume (no bookkeeping) applies everything and ends clean at max.
func TestUpFreshDatabase(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: database.NilVersion, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	if err := upFailClosed(m); err != nil {
		t.Fatalf("fresh up failed: %v", err)
	}
	if drv.CurrentVersion != latestMigrationVersion || drv.IsDirty {
		t.Errorf("expected clean version %d, got v=%d dirty=%v", latestMigrationVersion, drv.CurrentVersion, drv.IsDirty)
	}
	if len(drv.MigrationSequence) != latestMigrationVersion {
		t.Errorf("expected %d migrations applied, got %d", latestMigrationVersion, len(drv.MigrationSequence))
	}
}

// An already-current database is a no-op.
func TestUpAlreadyCurrent(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: latestMigrationVersion, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	if err := upFailClosed(m); err != nil {
		t.Fatalf("no-op up failed: %v", err)
	}
	if len(drv.MigrationSequence) != 0 {
		t.Errorf("expected no migrations run, got %d", len(drv.MigrationSequence))
	}
}

// The configured database must be disposable: Up applies the same embedded
// migration path used by cmd/server. CI/local validation opts in explicitly.
func TestEmbeddedMigrationsAgainstConfiguredDatabase(t *testing.T) {
	databaseURL := os.Getenv("MIGRATION_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MIGRATION_TEST_DATABASE_URL is not set")
	}
	if err := Up(databaseURL); err != nil {
		t.Fatalf("apply embedded migrations: %v", err)
	}
}

// failingDriver makes Run fail so we can assert non-dirty errors propagate.
type failingDriver struct {
	stub.Stub
	runErr error
}

func (f *failingDriver) Run(migration io.Reader) error { return f.runErr }

func TestUpNonDirtyErrorPropagates(t *testing.T) {
	drv := &failingDriver{
		Stub:   stub.Stub{CurrentVersion: database.NilVersion, MigrationSequence: []string{}},
		runErr: errors.New("permission denied for schema public"),
	}
	m := newTestMigrate(t, drv)

	err := upFailClosed(m)
	if err == nil {
		t.Fatal("expected error for non-dirty failure")
	}
	if !strings.Contains(err.Error(), "migrate up") {
		t.Errorf("expected wrapped migrate up error, got %v", err)
	}
}
