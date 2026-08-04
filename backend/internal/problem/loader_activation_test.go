package problem

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCatalogActivationCopySharesOneShotState(t *testing.T) {
	t.Run("commit then abort", func(t *testing.T) {
		loader, activation := prepareTestCatalogActivation(t)
		copied := *activation

		if err := copied.Commit(); err != nil {
			t.Fatalf("commit copied activation: %v", err)
		}
		if err := activation.Abort(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("abort original activation error = %v, want ErrCatalogActivationConsumed", err)
		}
		if got := catalogGeneration(loader); got != 1 {
			t.Fatalf("catalog generation = %d, want 1", got)
		}
		assertPublicationLockReleased(t, loader)
	})

	t.Run("abort then commit", func(t *testing.T) {
		loader, activation := prepareTestCatalogActivation(t)
		copied := *activation

		if err := copied.Abort(); err != nil {
			t.Fatalf("abort copied activation: %v", err)
		}
		if err := activation.Commit(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("commit original activation error = %v, want ErrCatalogActivationConsumed", err)
		}
		if got := catalogGeneration(loader); got != 0 {
			t.Fatalf("catalog generation = %d, want 0", got)
		}
		assertPublicationLockReleased(t, loader)
	})
}

func TestCatalogActivationRejectsRepeatedConsumption(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		loader, activation := prepareTestCatalogActivation(t)

		if err := activation.Commit(); err != nil {
			t.Fatalf("first commit: %v", err)
		}
		if err := activation.Commit(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("second commit error = %v, want ErrCatalogActivationConsumed", err)
		}
		if err := activation.Abort(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("abort after commit error = %v, want ErrCatalogActivationConsumed", err)
		}
		if got := catalogGeneration(loader); got != 1 {
			t.Fatalf("catalog generation = %d, want 1", got)
		}
	})

	t.Run("abort", func(t *testing.T) {
		loader, activation := prepareTestCatalogActivation(t)

		if err := activation.Abort(); err != nil {
			t.Fatalf("first abort: %v", err)
		}
		if err := activation.Abort(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("second abort error = %v, want ErrCatalogActivationConsumed", err)
		}
		if err := activation.Commit(); !errors.Is(err, ErrCatalogActivationConsumed) {
			t.Fatalf("commit after abort error = %v, want ErrCatalogActivationConsumed", err)
		}
		if got := catalogGeneration(loader); got != 0 {
			t.Fatalf("catalog generation = %d, want 0", got)
		}
	})
}

func TestCatalogActivationConcurrentCommitAndAbort(t *testing.T) {
	loader, activation := prepareTestCatalogActivation(t)
	copied := *activation
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- activation.Commit()
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- copied.Abort()
	}()
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, consumed int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrCatalogActivationConsumed):
			consumed++
		default:
			t.Fatalf("unexpected activation result: %v", err)
		}
	}
	if succeeded != 1 || consumed != 1 {
		t.Fatalf("activation results: succeeded=%d consumed=%d, want 1 each", succeeded, consumed)
	}
	if got := catalogGeneration(loader); got > 1 {
		t.Fatalf("catalog published %d generations, want at most 1", got)
	}
	assertPublicationLockReleased(t, loader)
}

func TestCatalogActivationRejectsInvalidToken(t *testing.T) {
	var nilActivation *catalogActivationToken
	if err := nilActivation.Commit(); !errors.Is(err, ErrInvalidCatalogActivation) {
		t.Fatalf("nil activation commit error = %v, want ErrInvalidCatalogActivation", err)
	}
	if err := nilActivation.Abort(); !errors.Is(err, ErrInvalidCatalogActivation) {
		t.Fatalf("nil activation abort error = %v, want ErrInvalidCatalogActivation", err)
	}
	activation := &catalogActivationToken{}
	if err := activation.Commit(); !errors.Is(err, ErrInvalidCatalogActivation) {
		t.Fatalf("zero activation commit error = %v, want ErrInvalidCatalogActivation", err)
	}
	if err := activation.Abort(); !errors.Is(err, ErrInvalidCatalogActivation) {
		t.Fatalf("zero activation abort error = %v, want ErrInvalidCatalogActivation", err)
	}
}

func prepareTestCatalogActivation(t *testing.T) (*GitLoader, *catalogActivationToken) {
	t.Helper()
	loader := newSnapshotRuntimeLoader(t, setupTestProblems(t))
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatalf("build catalog candidate: %v", err)
	}
	activation, err := loader.prepareActivation(candidate)
	if err != nil {
		t.Fatalf("prepare catalog activation: %v", err)
	}
	return loader, activation
}

func catalogGeneration(loader *GitLoader) uint64 {
	loader.catalogMu.RLock()
	defer loader.catalogMu.RUnlock()
	return loader.generation
}

func assertPublicationLockReleased(t *testing.T, loader *GitLoader) {
	t.Helper()
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatalf("build next catalog candidate: %v", err)
	}
	activation, err := loader.prepareActivation(candidate)
	if err != nil {
		t.Fatalf("publication lock was not reusable: %v", err)
	}
	if err := activation.Abort(); err != nil {
		t.Fatalf("abort lock probe activation: %v", err)
	}
}
