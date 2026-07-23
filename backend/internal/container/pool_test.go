package container

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type mockManager struct {
	mu        sync.Mutex
	nextID    int
	created   []string
	removed   []string
	failReady bool
}

func (m *mockManager) Create(ctx context.Context, opts CreateOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("c%d", m.nextID)
	m.created = append(m.created, id)
	return id, nil
}

func (m *mockManager) Exec(ctx context.Context, containerID string, cmd []string) (ExecResult, error) {
	return ExecResult{ExitCode: 0}, nil
}

func (m *mockManager) ExecInteractive(ctx context.Context, containerID string, cmd []string) (TerminalSession, error) {
	return nil, nil
}

func (m *mockManager) Remove(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, containerID)
	return nil
}

func (m *mockManager) Logs(ctx context.Context, containerID string) (string, error) {
	return "", nil
}

func (m *mockManager) WaitReady(ctx context.Context, containerID string, check func() bool, timeout time.Duration) error {
	if m.failReady {
		return fmt.Errorf("not ready")
	}
	if check != nil {
		check()
	}
	return nil
}

func (m *mockManager) IsRunning(ctx context.Context, containerID string) (bool, error) {
	return true, nil
}

func (m *mockManager) removedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.removed)
}

func waitForPool(t *testing.T, p *Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Len() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pool did not reach size %d, got %d", want, p.Len())
}

func TestPoolFillsAndAcquires(t *testing.T) {
	mgr := &mockManager{}
	p := NewPool(mgr, "k3s-base:latest", 2)
	defer p.Drain(context.Background())

	waitForPool(t, p, 2)

	id1, ok := p.Acquire()
	if !ok || id1 == "" {
		t.Fatalf("expected first acquire to succeed, got id=%q ok=%v", id1, ok)
	}
	id2, ok := p.Acquire()
	if !ok || id2 == "" {
		t.Fatalf("expected second acquire to succeed, got id=%q ok=%v", id2, ok)
	}
	if id1 == id2 {
		t.Fatalf("expected distinct container ids, got %q twice", id1)
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("expected acquire from empty pool to fail")
	}
}

func TestPoolAcquireEmptyWhenSizeZero(t *testing.T) {
	mgr := &mockManager{}
	p := NewPool(mgr, "k3s-base:latest", 0)
	defer p.Drain(context.Background())

	if _, ok := p.Acquire(); ok {
		t.Fatal("expected acquire from zero-size pool to fail")
	}
	if len(mgr.created) != 0 {
		t.Fatalf("expected no containers created for zero-size pool, got %d", len(mgr.created))
	}
}

func TestPoolDrainRemovesContainers(t *testing.T) {
	mgr := &mockManager{}
	p := NewPool(mgr, "k3s-base:latest", 2)

	waitForPool(t, p, 2)
	p.Drain(context.Background())

	if p.Len() != 0 {
		t.Fatalf("expected empty pool after drain, got %d", p.Len())
	}
	if got := mgr.removedCount(); got != 2 {
		t.Fatalf("expected 2 removed containers after drain, got %d", got)
	}
}

func TestPoolFillFailureKeepsPoolEmpty(t *testing.T) {
	mgr := &mockManager{failReady: true}
	p := NewPool(mgr, "k3s-base:latest", 1)
	defer p.Drain(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.removedCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if p.Len() != 0 {
		t.Fatalf("expected empty pool when warm containers fail readiness, got %d", p.Len())
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("expected acquire to fail when readiness fails")
	}
}
