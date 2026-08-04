# K8s Quiz — Implementation Plan

> 웹 기반 Kubernetes 트러블슈팅 플랫폼. 사용자에게 고장난 k3s 환경을 제공하고, 웹 터미널에서 문제를 해결한 뒤 검증 스크립트로 판정한다.

## Tech Stack

| Layer | Choice |
|-------|--------|
| Backend | Go + Gin |
| Frontend | React + Vite + TypeScript |
| UI | Tailwind CSS + shadcn/ui |
| State | Zustand |
| Terminal | xterm.js |
| DB | PostgreSQL |
| Auth | GitHub OAuth → JWT (Access 15m + Refresh 7d) via httpOnly cookies |
| API | REST + WebSocket |
| Infra | Docker Compose, Docker Socket mount |
| K8s | k3s-in-Docker (user-per-container) |
| CI/CD | GitHub Actions (build+test), deploy manual |

## Architecture Decisions

### Container Lifecycle
- User clicks "Start Problem" → Docker container created (k3s-base or custom image) → k3s boots → setup.sh runs → WebSocket pushes stage progress → terminal ready
- Timeout: per-problem configurable (default 30min), auto-cleanup on expiry
- Environment reset: recreate container, keep timeout
- Concurrent sessions: 1 active logical session per user; a fresh start is rejected
  while one is active, and reset creates the next generation after old-generation cleanup
- Server restart: cleanup all containers
- Container crash: mark failed, prompt user to reset
- Local Docker networking: a per-allocation bridge reduces accidental peer discovery,
  but it is development-only and does not prove host/LAN/egress isolation

### ContainerManager Interface
```go
type ContainerManager interface {
    Create(ctx context.Context, opts CreateOpts) (string, error)
    Exec(ctx context.Context, containerID string, cmd []string) (ExecResult, error)
    ExecInteractive(ctx context.Context, containerID string, cmd []string) (io.ReadWriteCloser, error)
    Remove(ctx context.Context, containerID string) error
    Logs(ctx context.Context, containerID string) (string, error)
    WaitReady(ctx context.Context, containerID string, check func() bool, timeout time.Duration) error
}
```
- Initial impl: Docker SDK (`github.com/docker/docker/client`)
- Future: K8s Pod, remote Docker host, container pool

### Verification
- Local-development legacy path: "Check" runs `verify.sh` via Docker exec. This is
  `development_guest` assurance only and is never eligible for public traffic.
- The current verify HTTP request is synchronous from the browser's perspective: it
  waits for a terminal grade or safe infrastructure failure and returns
  `{success,log}` on `200`. Durable lifecycle events independently publish the same
  allowlisted outcome for replay/reconnect.
- Public path: provider-neutral `VerifyRequest` → learner-external trusted verifier →
  authenticated receipt + allowlisted feedback. See
  `docs/architecture/adr/001-trusted-verifier-control-plane.md`.
- verify_type: `script` (verify.sh exit code), `choice` (multiple choice matching)
- `text` grading (LLM) — future

### Problem Definition (separate Git repo)
```
k8s-quiz-problems/
  pod-crashloop/
    problem.yaml
    Dockerfile        # only for custom image problems
    setup.sh          # creates broken state
    verify.sh         # verification script
    hint.md           # optional
  network-dns-fail/
    ...
```

`problem.yaml` schema:
```yaml
id: pod-crashloop
title: "CrashLoopBackOff 해결"
description: |
  Pod이 CrashLoopBackOff 상태입니다. 원인을 파악하고 수정하세요.
category: pod          # pod | network | storage | rbac | scheduling | config
difficulty: easy       # easy | medium | hard
type: fix              # fix | find
timeout_minutes: 30
verify_type: script    # script | choice
base_image: k3s-base:latest   # OR use `image:` for custom
# image: custom-problem:v1    # alternative to base_image
choices:               # only for verify_type: choice
  - id: a
    text: "OOMKilled"
  - id: b
    text: "ImagePullBackOff"
  - id: c
    text: "CrashLoopBackOff due to missing ConfigMap"
  - id: d
    text: "Node NotReady"
correct_choice: c      # only for verify_type: choice
```

- Loading: the backend validates the already checked-out local submodule; operators
  update it explicitly with `make problems-update`. Runtime Sync does not clone or
  pull. Startup bootstraps an empty PostgreSQL ledger or adopts an exactly matching
  persisted head; a changed checkout requires explicit admin Sync and is never
  published implicitly by restart. Sync appends a globally ordered generation with
  head CAS, and fresh durable sessions bind that `catalog_generation` plus the exact
  problem revision. Canonical runtime bytes are stored in a local filesystem CAS and
  immutably bound to the revision in PostgreSQL, so ordinary restart and historical
  resolution no longer depend on the checkout. Public list/detail and new-session
  admission read the append-only `problem_catalog_head → problem_catalog_entries →
  problem_artifacts` chain directly. The mutable `problems` table is only an admin/
  identity compatibility projection; changing it cannot rewrite the published public
  catalog, and Upsert/Delete treat any ID present in catalog entries as managed.
  Signed provenance, separate approval, revocation, replicated/backup recovery, and a
  public release policy are still required before public service.
- k3s-base image: `rancher/k3s` + kubectl + common tools pre-installed

### Auth
- GitHub OAuth → JWT Access(15min) + Refresh(7d), delivered as **httpOnly cookies**
  (`access_token` Path=/, `refresh_token` Path=/api/auth; SameSite=Lax, Secure on
  https). `Authorization: Bearer` remains accepted for scripts/tooling.
- OAuth callback sets the cookies and redirects to the frontend (no tokens in URL).
- Refresh rotation with token families: reuse of a used refresh token revokes the
  whole family (replay detection).
- `POST /api/auth/dev-login` (local dev only): exchanges a validly-signed access
  JWT for cookies, preserving the dev-admin browser flow without an auth bypass.
- Roles: `admin` (metadata-draft CRUD, atomic runtime-catalog sync, user mgmt,
  progress view), `user` (solve active problems);
  demoting the last admin is rejected.
- WebSocket auth: the httpOnly cookie authenticates the upgrade (browsers); the
  legacy first-message JWT remains a fallback for non-browser clients.
- Migrations: embedded in the backend and applied at boot (golang-migrate);
  first-boot compose seeding via `docker/db-init/`.
- Libraries: `golang-jwt/jwt`, `golang-migrate/migrate`, manual OAuth2

### API Design (REST + WebSocket)

```
GET    /api/auth/github          # initiate OAuth
GET    /api/auth/github/callback # OAuth callback → sets httpOnly cookies, redirects
POST   /api/auth/dev-login       # local dev only: signed JWT → cookies
POST   /api/auth/refresh         # refresh access token
POST   /api/auth/logout          # revoke refresh family + clear cookies; 204
DELETE /api/auth/logout          # compatibility alias with the same behavior
GET    /api/auth/me              # current user info

GET    /api/problems             # list (filter: category, difficulty, type)
GET    /api/problems/:id         # detail
POST   /api/problems/:id/start   # Idempotency-Key required; 200 snapshot, 429 while another user transition runs
POST   /api/problems/:id/reset   # 200 ready/in-progress snapshot, or 202 cleanup_pending snapshot
POST   /api/problems/:id/verify  # synchronous final result: 200 {success,log}
POST   /api/problems/:id/submit  # exact choice operation; body binds session generation

GET    /api/sessions/current     # current active session
POST   /api/sessions/end         # exact {session_id,generation}; 204 absent proof or 202 cleanup_pending

GET    /api/users/me/attempts    # my attempt history
GET    /api/users/me/progress    # my progress summary
GET    /api/users/me/achievements # my achievements
GET    /api/leaderboard          # leaderboard

# Admin
GET    /api/admin/problems       # list all
POST   /api/admin/problems       # create inactive metadata draft only
PUT    /api/admin/problems/:id   # update inactive metadata draft only
DELETE /api/admin/problems/:id   # delete inactive metadata draft only
POST   /api/admin/problems/sync  # validate + atomically activate current checkout; no git pull
GET    /api/admin/users          # list users
PUT    /api/admin/users/:id/role # change role
GET    /api/admin/attempts       # all attempts (filter by user/problem)

# WebSocket
WS     /ws/terminal              # exact session_id+generation terminal stream
WS     /ws/lifecycle             # PostgreSQL-backed replayable lifecycle authority
```

`POST /api/sessions/end` requires `Idempotency-Key: end:<uuid>` and an exact
`{"session_id":"...","generation":n}` body. `204` means the logical session's
allocations are proven absent. `202` returns
`{request_id,session_id,generation,status:"cleanup_pending"}` and the browser retries
with the same key until it receives 204 or a definitive rejection. A stale identity,
generation, or conflicting key returns 409. The removed
`DELETE /api/sessions/current` route must not be restored because it cannot bind a
retry to an exact generation.

`POST /api/problems/:id/submit` requires `Idempotency-Key` and an exact
`{"session_id":"...","generation":n,"choice_id":"..."}` body. A successful
submission returns
`{request_id,problem_id,session_id,generation,success}`. Invalid input or an ordinary
submission rejection returns `400`, stale generation/key/lifecycle identity returns
`409`, throttle or another transition returns `429`, and shutdown returns `503`.

Implemented terminal WebSocket protocol (the public deployment gate still also
requires the private isolated Runner boundary described below):
```json
// Non-browser compatibility only. Browser upgrades require a valid httpOnly
// access_token cookie and are rejected with 401 before upgrade when invalid.
{"type": "auth", "token": "jwt..."}

// Server → Client after provider open and authority validation. attach_nonce is
// unpredictable and unique to this candidate socket.
{"type": "terminal_attached", "session_id": "uuid", "generation": 2, "attach_nonce": "random"}

// Client → Server; exact nonce acknowledgement.
{"type": "terminal_ready", "attach_nonce": "random"}

// Client → Server after terminal_ready; every terminal mutation echoes the nonce.
{"type": "input", "attach_nonce": "random", "data": "kubectl get pods\n"}
{"type": "resize", "attach_nonce": "random", "cols": 120, "rows": 40}

// Server → Client only after the candidate commits Hub ownership.
{"type": "output", "data": "..."}

// Server → Client (error)
{"type": "error", "message": "..."}
```

The terminal socket never carries lifecycle state. `/ws/lifecycle` is the only live
lifecycle authority and replays `k8s-quiz.lifecycle/v1` snapshots/events from the
durable cursor. HTTP `GET /api/sessions/current` and lifecycle frames are fenced by a
frontend authority epoch so a late HTTP response cannot resurrect or regress a newer
WebSocket state. The target terminal handshake opens the provider, rechecks exact
session/controller/auth authority, sends `terminal_attached` with an unpredictable
`attach_nonce`, and waits for an exact `terminal_ready`. Frames received before the
matching ready are discarded in receive order and never replayed; every later
input/resize must echo the nonce. Only after ready and a final authority check may an
irreversible Hub compare-and-swap commit the candidate and close the old tab with code
4001. Failures before that commit preserve the old owner; the contract makes no
rollback promise after commit. Browser WS authority is bounded by JWT `exp`: expiry
closes the socket, and reconnect first uses the shared auth probe/refresh path.

### DB Schema

```sql
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_id BIGINT UNIQUE NOT NULL,
    username VARCHAR(255) NOT NULL,
    email VARCHAR(255),
    avatar_url TEXT,
    role VARCHAR(20) NOT NULL DEFAULT 'user', -- 'admin' | 'user'
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE problems (
    id VARCHAR(255) PRIMARY KEY, -- from problem.yaml id
    title VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    category VARCHAR(50) NOT NULL,
    difficulty VARCHAR(20) NOT NULL,
    type VARCHAR(20) NOT NULL, -- 'fix' | 'find'
    timeout_minutes INT NOT NULL DEFAULT 30,
    verify_type VARCHAR(20) NOT NULL DEFAULT 'script', -- 'script' | 'choice'
    base_image VARCHAR(255),
    image VARCHAR(255),
    choices JSONB, -- for choice type
    correct_choice VARCHAR(10), -- for choice type
    hint TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    problem_id VARCHAR(255) NOT NULL REFERENCES problems(id),
    status VARCHAR(20) NOT NULL DEFAULT 'in_progress', -- 'in_progress' | 'success' | 'failed' | 'timeout'
    container_id VARCHAR(255),
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    duration_seconds INT,
    verify_log TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    token_hash VARCHAR(255) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    family_id UUID NOT NULL DEFAULT gen_random_uuid(),  -- 004: rotation family
    used BOOLEAN NOT NULL DEFAULT false                 -- 004: reuse detection
);

CREATE INDEX idx_attempts_user ON attempts(user_id);
CREATE INDEX idx_attempts_problem ON attempts(problem_id);
CREATE INDEX idx_attempts_status ON attempts(status);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens(user_id);
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens(family_id);
```

The current migrations additionally persist an append-only problem catalog ledger:
`problem_catalog_publications` is the generation chain,
`problem_catalog_entries` stores each generation's exact metadata selection, and the
singleton `problem_catalog_head` advances by CAS. `sessions.catalog_generation` binds
every fresh durable reservation to the head entry selected at admission. Database
triggers reject mutation/deletion of sealed publications and reject a new session
whose selection is not the current artifact-bound head. `problem_artifacts` binds
each revision to canonical runtime bytes in the configured filesystem CAS. Public
list/detail and admission resolve through that head/entry/artifact chain, not through
the mutable `problems` row. The `problems` table remains an admin/identity compatibility
projection and cannot override membership in the append-only ledger. The local CAS
provides integrity and checkout-free recovery, but signatures, publisher/build
provenance, approval, revocation, expiry, and production artifact availability are not
implied and remain separate public-service gates.

### Frontend Pages

```
/                     → Dashboard (problem list, category+difficulty filter)
/login                → GitHub OAuth login
/problems/:id         → Problem solving (terminal + description + verify button + reset)
/profile              → My progress, attempt history
/admin                → Admin dashboard (metadata drafts, catalog sync, user mgmt, attempts)
/admin/problems/:id   → Problem create/edit
/docs                 → User guide + Problem creation guide
```

### Frontend Structure
```
frontend/
  src/
    api/           # axios/fetch client, WebSocket client
    components/    # shared UI components
      Terminal/    # xterm.js wrapper
      Layout/
      ui/          # shadcn/ui components
    pages/
      Dashboard/
      Login/
      Problem/
      Profile/
      Admin/
      Docs/
    stores/        # zustand stores (auth, session, terminal)
    hooks/
    types/
    App.tsx
    main.tsx
  index.html
  vite.config.ts
  tailwind.config.ts
  tsconfig.json
  package.json
  Dockerfile
```

## Project Structure (Code Repo)

```
k8s-quiz/
  backend/
    cmd/server/main.go
    internal/
      auth/
        handler.go      # OAuth endpoints, refresh
        service.go      # JWT issue/verify, OAuth flow
        middleware.go   # auth middleware for Gin
        repository.go   # refresh token storage
      user/
        handler.go
        service.go
        repository.go
        model.go
      problem/
        handler.go      # CRUD + list + start/verify
        service.go
        repository.go
        model.go
        loader.go       # ProblemLoader interface + Git impl
      session/
        handler.go
        service.go      # session lifecycle, timeout mgmt
        repository.go
        model.go
      container/
        manager.go      # ContainerManager interface
        docker.go       # Docker SDK implementation
        model.go
      ws/
        hub.go          # WebSocket connection hub
        terminal.go     # terminal handler (docker exec bridge)
        protocol.go     # message types
    pkg/
      config/
        config.go       # env var loading
    migrations/
      001_init.up.sql
      001_init.down.sql
    Dockerfile
    go.mod
  frontend/
    (see above)
  docker/
    k3s-base/
      Dockerfile        # k3s-base image
  docker-compose.yaml
  .github/
    workflows/
      ci.yaml
  .env.example
  PLAN.md
  AGENTS.md
```

## Docker Compose

```yaml
services:
  backend:
    build: ./backend
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./problems:/problems
      - problem_artifacts:/var/lib/k8s-quiz/problem-artifacts
    environment:
      - DATABASE_URL=postgres://k8squiz:k8squiz@db:5432/k8squiz?sslmode=disable
      - GITHUB_CLIENT_ID=${GITHUB_CLIENT_ID}
      - GITHUB_CLIENT_SECRET=${GITHUB_CLIENT_SECRET}
      - JWT_SECRET=${JWT_SECRET}
      - JWT_REFRESH_SECRET=${JWT_REFRESH_SECRET}
      - PROBLEMS_REPO_PATH=/problems
      - PROBLEM_ARTIFACT_STORE_PATH=/var/lib/k8s-quiz/problem-artifacts
      - FRONTEND_URL=http://localhost:5173
      - DEPLOYMENT_MODE=development
      - SERVER_BIND_MODE=compose-private
      - SERVER_HOST=backend
      - RUNNER_PROVIDER=local-docker
      - RUNNER_SCOPE=${RUNNER_SCOPE}
    depends_on:
      db:
        condition: service_healthy
    restart: unless-stopped

  frontend:
    build: ./frontend
    ports:
      - "127.0.0.1:5173:80"
    depends_on:
      - backend

  db:
    image: postgres:16-alpine
    environment:
      - POSTGRES_USER=k8squiz
      - POSTGRES_PASSWORD=k8squiz
      - POSTGRES_DB=k8squiz
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./backend/migrations:/migrations:ro
      - ./docker/db-init:/docker-entrypoint-initdb.d:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U k8squiz"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  # Image builder (profile "image", exits immediately):
  #   docker compose --profile image up --build k3s-base
  k3s-base:
    image: k3s-base:latest
    build: ./docker/k3s-base
    profiles: ["image"]
    entrypoint: ["true"]
    restart: "no"

volumes:
  pgdata:
  problem_artifacts:
```

## Environment Variables

```
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
JWT_SECRET=
JWT_REFRESH_SECRET=
DATABASE_URL=postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable
PROBLEMS_REPO_PATH=./problems
PROBLEM_ARTIFACT_STORE_PATH=./data/problem-artifacts
FRONTEND_URL=http://localhost:5173
DEPLOYMENT_MODE=development
SERVER_BIND_MODE=loopback
SERVER_HOST=127.0.0.1
RUNNER_PROVIDER=local-docker
RUNNER_SCOPE=replace-with-a-unique-project-scope
DOCKER_HOST=unix:///var/run/docker.sock
SERVER_PORT=8080
MAX_CONCURRENT_SESSIONS=0   # local dev may use 0; public mode requires a positive cap (429 above)
```

## Implementation Order

### Phase 1: Scaffolding
- [ ] Go module init, Gin server, config loading
- [ ] React + Vite + TS + Tailwind + shadcn/ui setup
- [ ] Docker Compose (backend, frontend, postgres)
- [ ] DB migrations (users, problems, attempts, refresh_tokens)
- [ ] AGENTS.md

### Phase 2: Auth
- [ ] GitHub OAuth flow (handler + service)
- [ ] JWT issue/verify/refresh
- [ ] Auth middleware for Gin
- [ ] Frontend login page + token storage + auth store

### Phase 3: Container Management
- [ ] ContainerManager interface + Docker impl
- [ ] k3s-base Dockerfile
- [ ] Container lifecycle: create, wait ready, exec, remove
- [ ] Resource limits (CPU, RAM, disk)
- [ ] Network isolation (per-user Docker network)
- [ ] Timeout + auto-cleanup goroutine

### Phase 4: WebSocket Terminal
- [x] Terminal WebSocket hub with exact owner/lease takeover and close code 4001
- [x] Cookie-authenticated browser upgrade and originless non-browser auth fallback
- [x] Unpredictable `attach_nonce` → exact `terminal_ready` handshake,
      nonce-bound input/resize, and pre-ready ordered discard
- [x] Provider open + ACK/ready + exact session transition commit lease before
      irreversible Hub CAS; JWT-expiry socket close and refresh-before-reconnect
- [x] xterm.js frontend component with bounded reconnect and stale-socket fences
- [x] PostgreSQL-backed lifecycle WebSocket with snapshot/event replay
- [x] Resize handling and bounded dimensions

### Phase 5: Problems
- [ ] ProblemLoader interface + Git implementation
- [ ] Metadata-draft CRUD API (admin); catalog-managed runtime rows are immutable here
- [ ] Active-only problem list/detail API (user)
- [x] Atomic problem sync endpoint for the already checked-out source (no git pull)
- [x] Append-only PostgreSQL catalog generation ledger with global CAS, safe startup
      bootstrap/exact adoption, ambiguous-commit reconciliation, and durable-session
      `catalog_generation` binding
- [x] Canonical runtime artifact codec, persistent local integrity CAS, immutable
      PostgreSQL artifact binding, and checkout-free exact-revision restart recovery
- [ ] Signed provenance, separate approval/revocation policy, and production artifact
      distribution/backup recovery gate
- [ ] Frontend: dashboard, problem detail page

### Phase 6: Verification
- [x] Local-development synchronous Verify endpoint (Docker exec,
      `development_guest` only; HTTP `200 {success,log}`)
- [ ] Public trusted verifier topology, worker, credential broker, receipt
      authentication, and adversarial acceptance suite
- [x] Exact generation-bound, idempotent choice submission endpoint
- [x] Durable lifecycle event replay (verification result uses allowlisted feedback)
- [ ] Attempt recording (status, duration, allowlisted public message; no raw evidence)
- [ ] Frontend: verify button, result display

### Phase 7: Session Management
- [x] Durable, generation-bound session start/reset/end operations
- [x] 1 concurrent session per user enforcement
- [ ] Timeout warning + auto-end
- [ ] Server restart cleanup
- [ ] Frontend: session state, timer display

### Phase 8: Frontend Polish
- [ ] Profile page (attempt history, progress)
- [ ] Admin pages (metadata drafts, catalog sync/status, user list, attempts)
- [ ] Docs page (user guide + problem creation guide)
- [ ] Responsive layout, error states

### Phase 9: CI/CD + Testing
- [ ] GitHub Actions: build + test on PR/push
- [ ] Backend unit tests (container manager, auth, verification)
- [ ] API integration tests (httptest)
- [ ] .env.example, README

## Key Libraries (Go)

- `github.com/gin-gonic/gin` — HTTP framework
- `github.com/gorilla/websocket` — WebSocket
- `github.com/docker/docker/client` — Docker SDK
- `github.com/golang-jwt/jwt/v5` — JWT
- `github.com/jackc/pgx/v5` — PostgreSQL driver
- `github.com/golang-migrate/migrate/v4` — DB migrations
- `golang.org/x/oauth2` — GitHub OAuth2
- `github.com/google/uuid` — UUID generation

## Key Libraries (Frontend)

- `react`, `react-dom`, `react-router-dom`
- `xterm`, `@xterm/addon-fit`, `@xterm/addon-web-links`
- `zustand`
- `tailwindcss`, `@shadcn/ui` (via CLI)
- `axios` or native fetch
- `vite`, `typescript`

## Security Notes

- Docker Socket: root-equivalent host authority; label scoping is not a sandbox, so
  this path is restricted to trusted local development
- Container: CPU, memory, and PID limits are enforced; durable disk quota is not yet implemented
- Network: per-allocation bridges reduce accidental peer discovery, but Local Docker
  has no proven host/LAN/egress isolation and is ineligible for public traffic
- JWT: short-lived access, hashed refresh tokens in DB
- Input: sanitize terminal input is NOT needed (it's a shell by design), but API inputs validated
- CORS: restrict to FRONTEND_URL

## Future (Not in v1)

- Text grading via LLM
- Docker image registry for problems (replace Git loader)
- Redis for WebSocket state (horizontal scaling)
- Multi-org / team support
- Deploy-it problem type
- Container pre-warm pool
