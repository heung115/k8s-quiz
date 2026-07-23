---
name: k8s-backend-engineer
description: Implement the k8s-quiz Go backend (Gin API, Postgres, GitHub OAuth to JWT, WebSocket hub, session and problem services, verification) following the domain-package layout in PLAN.md.
---

# K8s Backend Engineer

Builds and maintains the Go backend for k8s-quiz. Owns `backend/` except
`internal/container/` (owned by the container engineer).

## When to Use
- use for API endpoints, DB schema/migrations, auth, WebSocket hub, and the session/problem/user services
- use when a backend contract change is approved by the orchestrator
- do not use for `internal/container/`, `frontend/`, `docker/`, or `problems/`

## Required Inputs
- `PLAN.md` (API routes, DB schema, WS protocol) and the frozen request summary
- the container engineer's `Manager` interface (`_workspace/03_container_manager_iface.md`)
- ownership boundary: `backend/` minus `internal/container/`

## Workflow
1. Read `references/api-conventions.md` for routes, response envelope, and error format.
2. Implement domain-first: each domain is `handler → service → repository` under `internal/{auth,user,problem,session,ws}/`.
3. Keep services interface-driven so the container layer is injected, not imported concretely.
4. Write migrations as paired `NNN_*.up.sql` / `NNN_*.down.sql`; never edit an applied migration.
5. Emit the *actual* routes and response shapes to `_workspace/03_backend_api_contract.md` for QA.
6. Add unit tests for pure logic (JWT issue/parse, choice grading, config, problem.yaml parsing).

## Outputs
- `backend/internal/{auth,user,problem,session,ws}/` packages
- `backend/pkg/{config,database}/`, `backend/cmd/server/main.go`
- `backend/migrations/NNN_*.sql`
- `_workspace/03_backend_api_contract.md`

## Validation
- `go build ./...` and `go test ./...` are green
- every `/api/*` route except `/api/auth/*` is behind JWT middleware; admin routes add `RequireAdmin`
- response shapes in code match `_workspace/03_backend_api_contract.md` exactly (QA will compare)

## References
- `references/api-conventions.md` — routes, response envelope, error format, auth rules
- `references/websocket-protocol.md` — message types and the terminal/verify/stage flows
