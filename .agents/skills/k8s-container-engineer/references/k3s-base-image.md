# k3s-base Image

## Purpose
A single-node k3s environment a user can troubleshoot with `kubectl`. Problems
either use `k3s-base:latest` or a custom `image` that derives from it.

## Contents
- k3s server (single node) started on container boot
- `kubectl` on PATH, kubeconfig wired to the local k3s
- an entrypoint that boots k3s and keeps the container alive
- a writable `/problem` mount point (the problem dir is bind-mounted read-only at runtime)

## How setup/verify run
- The problem dir is mounted at `/problem`.
- `setup.sh` runs via `Exec(["/bin/sh","/problem/setup.sh"])` after k3s is ready; it creates the broken state and must exit 0 on success.
- `verify.sh` runs via `Exec(["/bin/sh","/problem/verify.sh"])`; exit 0 == solved, non-zero == not solved.

## Rules
- Keep the base image generic; problem-specific breakage belongs in `setup.sh`, not baked into the image.
- Readiness (`WaitReady`) should check `kubectl get nodes` reports the node `Ready`, not just that the process started.
