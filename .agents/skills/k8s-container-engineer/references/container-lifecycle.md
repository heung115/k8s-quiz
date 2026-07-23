# Container Lifecycle

## States (from PLAN.md)
`Start Problem` → create container (k3s-base or custom image) → k3s boots →
`setup.sh` runs (creates the broken state) → `stage` progress pushed → terminal
ready.

## Manager Interface (stable contract)
```go
type Manager interface {
    Create(ctx, opts CreateOpts) (string, error)
    Exec(ctx, containerID string, cmd []string) (ExecResult, error)
    ExecInteractive(ctx, containerID string, cmd []string) (io.ReadWriteCloser, error)
    Remove(ctx, containerID string) error
    Logs(ctx, containerID string) (string, error)
    WaitReady(ctx, containerID string, check func() bool, timeout time.Duration) error
}
```
Keep this surface minimal. New capabilities are added here first, then consumed
by the backend.

## Policies
- **Concurrency:** 1 session per user; starting a new problem removes the previous container.
- **Timeout:** per-problem (default 30m); a background reaper removes expired containers and ends the attempt as `timeout`.
- **Server restart:** cleanup all containers labeled `k8squiz.*` on boot.
- **Crash:** if the container dies mid-session, mark the attempt `failed` and prompt the user to reset.
- **Reset:** recreate the container from the same image, re-run `setup.sh`, keep the timeout window.
- **Isolation:** each container on its own Docker network; no inter-container access.
- **Limits:** 1 CPU (`NanoCPUs=1e9`), 1 GiB RAM (`Memory=1<<30`).

## Removable Heuristics (delete when stable)
- retry `image pull` twice on transient timeout — remove once images are pre-cached
- poll interval tuning for `WaitReady` — remove once k3s boot time is characterized
