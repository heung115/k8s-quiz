package container

import (
	"context"
	"io"
	"time"
)

type CreateOpts struct {
	Name        string
	Image       string
	Labels      map[string]string
	Env         []string
	CPULimit    int64
	MemoryLimit int64
	NetworkMode string
	Privileged  bool
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// OwnedAllocationObservation is the provider-private result of inspecting one
// deterministic container/network pair. OwnershipComplete is false whenever a
// present resource does not carry the exact immutable ownership label set.
// Callers must never mutate a resource when it is false.
type OwnedAllocationObservation struct {
	ContainerPresent  bool
	NetworkPresent    bool
	OwnershipComplete bool
}

// OwnedAllocationHandle is provider-private physical identity captured at
// create time. Names are discovery aids only; mutation is authorized solely
// against these immutable IDs plus the approved create specification.
type OwnedAllocationHandle struct {
	ContainerID string
	NetworkID   string
}

type OwnedAllocationTarget struct {
	Handle OwnedAllocationHandle
	Create CreateOpts
}

type OwnedAllocationRemoval struct {
	Before OwnedAllocationObservation
	After  OwnedAllocationObservation
}

// OwnedAllocationManager is the stronger LocalDockerRunner capability. The
// ordinary Manager methods remain for the development warm pool, but Runner
// lifecycle code must never fall back to name-only removal.
type OwnedAllocationManager interface {
	CreateOwnedAllocation(context.Context, CreateOpts) (OwnedAllocationHandle, error)
	InspectOwnedAllocation(context.Context, OwnedAllocationTarget) (OwnedAllocationObservation, error)
	RemoveOwnedAllocation(context.Context, OwnedAllocationTarget) (OwnedAllocationRemoval, error)
}

type TerminalSession interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

type Manager interface {
	Create(ctx context.Context, opts CreateOpts) (string, error)
	Exec(ctx context.Context, containerID string, cmd []string) (ExecResult, error)
	ExecInteractive(ctx context.Context, containerID string, cmd []string) (TerminalSession, error)
	Remove(ctx context.Context, containerID string) error
	Logs(ctx context.Context, containerID string) (string, error)
	WaitReady(ctx context.Context, containerID string, check func() bool, timeout time.Duration) error
	IsRunning(ctx context.Context, containerID string) (bool, error)
}
