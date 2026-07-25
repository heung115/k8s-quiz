package dbmigrate

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/stub"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/k8s-quiz/backend/migrations"
)

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
	if v != 4 {
		t.Errorf("expected latest version 4, got %d", v)
	}
}

// --- dirty-database self-heal (legacy volume recovery) ---

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

// A legacy volume: full 001–003 schema, bookkeeping stuck dirty at v1.
// upWithDirtyHeal must force v1 clean and converge to the latest version.
func TestUpRecoversFromDirtyDatabase(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: 1, IsDirty: true, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	if err := upWithDirtyHeal(m); err != nil {
		t.Fatalf("expected dirty recovery to succeed, got %v", err)
	}
	if drv.CurrentVersion != 4 {
		t.Errorf("expected final version 4, got %d", drv.CurrentVersion)
	}
	if drv.IsDirty {
		t.Error("expected database clean after recovery")
	}
	// After Force(1), migrations 2–4 were (idempotently) applied.
	if len(drv.MigrationSequence) != 3 {
		t.Errorf("expected 3 migrations replayed after force, got %d", len(drv.MigrationSequence))
	}
}

// A fresh volume (no bookkeeping) applies everything and ends clean at max.
func TestUpFreshDatabase(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: database.NilVersion, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	if err := upWithDirtyHeal(m); err != nil {
		t.Fatalf("fresh up failed: %v", err)
	}
	if drv.CurrentVersion != 4 || drv.IsDirty {
		t.Errorf("expected clean version 4, got v=%d dirty=%v", drv.CurrentVersion, drv.IsDirty)
	}
	if len(drv.MigrationSequence) != 4 {
		t.Errorf("expected 4 migrations applied, got %d", len(drv.MigrationSequence))
	}
}

// An already-current database is a no-op.
func TestUpAlreadyCurrent(t *testing.T) {
	drv := &stub.Stub{CurrentVersion: 4, MigrationSequence: []string{}}
	m := newTestMigrate(t, drv)

	if err := upWithDirtyHeal(m); err != nil {
		t.Fatalf("no-op up failed: %v", err)
	}
	if len(drv.MigrationSequence) != 0 {
		t.Errorf("expected no migrations run, got %d", len(drv.MigrationSequence))
	}
}

func TestDirtyVersionExtraction(t *testing.T) {
	if v, ok := dirtyVersion(migrate.ErrDirty{Version: 3}); !ok || v != 3 {
		t.Errorf("ErrDirty{3}: got (%d,%v)", v, ok)
	}
	wrapped := fmt.Errorf("failure: %w", migrate.ErrDirty{Version: 1})
	if v, ok := dirtyVersion(wrapped); !ok || v != 1 {
		t.Errorf("wrapped ErrDirty{1}: got (%d,%v)", v, ok)
	}
	msg := errors.New("Dirty database version 2. Fix and force version.")
	if v, ok := dirtyVersion(msg); !ok || v != 2 {
		t.Errorf("message fallback: got (%d,%v)", v, ok)
	}
	if _, ok := dirtyVersion(errors.New("some other failure")); ok {
		t.Error("non-dirty error must not be detected as dirty")
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

	err := upWithDirtyHeal(m)
	if err == nil {
		t.Fatal("expected error for non-dirty failure")
	}
	if !strings.Contains(err.Error(), "migrate up") {
		t.Errorf("expected wrapped migrate up error, got %v", err)
	}
}
