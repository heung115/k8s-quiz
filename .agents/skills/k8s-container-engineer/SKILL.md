---
name: k8s-container-engineer
description: Implement the k8s-quiz ContainerManager abstraction and its Docker Engine implementation, the k3s-base image, and container lifecycle (resource limits, network isolation, readiness, timeouts, cleanup).
---

# K8s Container Engineer

Owns the container runtime layer: the `Manager` interface, its Docker
implementation, and the k3s-base image. Owns `backend/internal/container/` and
`docker/`. Nothing else.

## When to Use
- use for the `Manager` interface, Docker SDK implementation, k3s-base image, and lifecycle policy
- use when the backend needs a new container capability (change the interface here, then notify backend)
- do not use for other `backend/internal/*` packages, `frontend/`, or `problems/`

## Required Inputs
- `PLAN.md` (ContainerManager interface, container lifecycle, security notes)
- the frozen request summary
- the problem image naming convention from the problem author (`base_image` / `image`)

## Workflow
1. Read `references/container-lifecycle.md` for create→ready→exec→remove and failure handling.
2. Keep `Manager` minimal and interface-only; the Docker SDK stays inside `docker.go`.
3. Enforce resource limits (1 CPU, 1 GiB RAM) and per-user network isolation on every `Create`.
4. Build the k3s-base image per `references/k3s-base-image.md`; problems either use it or a custom `image`.
5. Publish the final interface + lifecycle notes to `_workspace/03_container_manager_iface.md` so the backend codes against a stable contract.

## Outputs
- `backend/internal/container/{manager.go,docker.go,model.go}`
- `docker/k3s-base/Dockerfile`
- `_workspace/03_container_manager_iface.md`

## Validation
- `go build ./...` green; the Docker SDK is contained to `docker.go`
- every created container has labels `k8squiz.user` / `k8squiz.problem`, CPU/memory limits, and an isolated network
- `WaitReady` polls with a bounded timeout; `Remove` is force and idempotent
- interface changes are reflected in `_workspace/03_container_manager_iface.md` before backend integration

## References
- `references/container-lifecycle.md` — lifecycle states, readiness, timeout/cleanup, crash handling
- `references/k3s-base-image.md` — base image contents and how setup.sh/verify.sh run inside it
