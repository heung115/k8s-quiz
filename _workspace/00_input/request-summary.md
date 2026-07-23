# Request Summary (frozen)

> Orchestrator snapshots the request here before any build work so downstream
> specialists share one frozen understanding. Edit only the orchestrator.

## Request
Build a reusable harness (orchestrator + specialist skills + team spec) for the
k8s-quiz platform, derived from `PLAN.md`, so the platform can be built and
maintained by a coordinated team of specialists with deterministic handoffs.

## In Scope
- Backend: Go + Gin, Postgres, GitHub OAuth → JWT, WebSocket hub, session and
  problem services, verification.
- Frontend: React + Vite + TS + Tailwind + shadcn/ui, xterm.js terminal,
  Zustand stores, API + WS clients.
- Infra: Docker Compose, Docker-socket-backed `ContainerManager`, k3s-base
  image, per-user network isolation, resource limits, timeouts.
- Content: problem definitions (`problem.yaml` + `setup.sh` + `verify.sh` +
  `hint.md`) loaded from a separate Git repo.

## Out of Scope (v1)
Leaderboard/achievements, LLM text grading, image-registry problem loader,
Redis-backed horizontal scaling, multi-org, deploy-it problem type, pre-warm
pool. (Listed in PLAN.md "Future".)

## Assumptions (narrowest reasonable)
- Problems repo is cloned to `PROBLEMS_REPO_PATH`; `GitLoader` pulls and parses.
- A running Docker daemon is required only for *live* container/k3s runs; build
  and unit tests do not require it.
- `PLAN.md` is the contract authority; where this harness and `PLAN.md` differ,
  `PLAN.md` wins.

## Acceptance Bar
- `go build ./...` and `go test ./...` green for the backend.
- `npm run build` green for the frontend.
- `docker compose up --build` brings up backend + frontend + db.
- At least one problem solvable end-to-end; `verify.sh` exit code drives grading.
