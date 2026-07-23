package container

import (
	"context"
	"io"
	"time"
)

type CreateOpts struct {
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
