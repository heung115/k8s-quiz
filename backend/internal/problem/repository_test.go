package problem

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type catalogSafeToRetryError struct{}

func (catalogSafeToRetryError) Error() string     { return "failed before send" }
func (catalogSafeToRetryError) SafeToRetry() bool { return true }

func TestClassifyCatalogCommitErrorKnownNonCommitsRemainRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "commit rollback", err: pgx.ErrTxCommitRollback},
		{name: "serialization failure", err: &pgconn.PgError{Code: "40001", Message: "serialization failure"}},
		{name: "deadlock", err: &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}},
		{name: "not sent", err: catalogSafeToRetryError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyCatalogCommitError(test.err)
			if errors.Is(err, ErrCatalogCommitUnknown) {
				t.Fatalf("known non-commit was classified as ambiguous: %v", err)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("classified error does not retain cause %v: %v", test.err, err)
			}
		})
	}
}

func TestClassifyCatalogCommitErrorAmbiguousOutcomePoisonsAdmission(t *testing.T) {
	err := classifyCatalogCommitError(errors.New("connection lost after commit was sent"))
	if !errors.Is(err, ErrCatalogCommitUnknown) {
		t.Fatalf("ambiguous commit error = %v, want ErrCatalogCommitUnknown", err)
	}
}
