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
| Auth | GitHub OAuth → JWT (Access 15m + Refresh 7d) |
| API | REST + WebSocket |
| Infra | Docker Compose, Docker Socket mount |
| K8s | k3s-in-Docker (user-per-container) |
| CI/CD | GitHub Actions (build+test), deploy manual |

## Architecture Decisions

### Container Lifecycle
- User clicks "Start Problem" → Docker container created (k3s-base or custom image) → k3s boots → setup.sh runs → WebSocket pushes stage progress → terminal ready
- Timeout: per-problem configurable (default 30min), auto-cleanup on expiry
- Environment reset: recreate container, keep timeout
- Concurrent sessions: 1 per user (new problem kills previous)
- Server restart: cleanup all containers
- Container crash: mark failed, prompt user to reset
- Network isolation: separate Docker network per user container

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
- "Check" button → backend runs `verify.sh` via docker exec → async → WebSocket pushes result
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

- Loading: Git clone/pull initially → Docker image registry later (ProblemLoader interface)
- k3s-base image: `rancher/k3s` + kubectl + common tools pre-installed

### Auth
- GitHub OAuth → JWT Access(15min) + Refresh(7d)
- Roles: `admin` (problem CRUD, user mgmt, progress view), `user` (solve problems)
- WebSocket auth: first message after connection carries JWT
- Libraries: `golang-jwt/jwt`, `markbates/goth` or manual OAuth2

### API Design (REST + WebSocket)

```
POST   /api/auth/github          # initiate OAuth
GET    /api/auth/github/callback # OAuth callback → returns tokens
POST   /api/auth/refresh         # refresh access token
GET    /api/auth/me              # current user info

GET    /api/problems             # list (filter: category, difficulty, type)
GET    /api/problems/:id         # detail
POST   /api/problems/:id/start   # start problem → creates container, returns session
POST   /api/problems/:id/reset   # reset environment
POST   /api/problems/:id/verify  # trigger verification (async)
POST   /api/problems/:id/submit  # submit choice answer (for find+choice type)

GET    /api/sessions/current     # current active session
DELETE /api/sessions/current     # end session

GET    /api/users/me/attempts    # my attempt history
GET    /api/users/me/progress    # my progress summary

# Admin
GET    /api/admin/problems       # list all
POST   /api/admin/problems       # create
PUT    /api/admin/problems/:id   # update
DELETE /api/admin/problems/:id   # delete
POST   /api/admin/problems/sync  # trigger git pull
GET    /api/admin/users          # list users
PUT    /api/admin/users/:id/role # change role
GET    /api/admin/attempts       # all attempts (filter by user/problem)

# WebSocket
WS     /ws/terminal              # terminal session (auth via first message)
```

WebSocket message protocol:
```json
// Client → Server (auth)
{"type": "auth", "token": "jwt..."}

// Client → Server (terminal input)
{"type": "input", "data": "kubectl get pods\n"}

// Client → Server (resize)
{"type": "resize", "cols": 120, "rows": 40}

// Server → Client (terminal output)
{"type": "output", "data": "..."}

// Server → Client (stage progress during setup)
{"type": "stage", "stage": "container_created|k3s_booting|setup_running|ready", "message": "..."}

// Server → Client (verification result)
{"type": "verify_result", "success": true, "message": "...", "log": "..."}

// Server → Client (timeout warning)
{"type": "timeout_warning", "remaining_seconds": 300}

// Server → Client (session ended)
{"type": "session_ended", "reason": "timeout|reset|server_restart"}

// Server → Client (error)
{"type": "error", "message": "..."}
```

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
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_attempts_user ON attempts(user_id);
CREATE INDEX idx_attempts_problem ON attempts(problem_id);
CREATE INDEX idx_attempts_status ON attempts(status);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens(user_id);
```

### Frontend Pages

```
/                     → Dashboard (problem list, category+difficulty filter)
/login                → GitHub OAuth login
/problems/:id         → Problem solving (terminal + description + verify button + reset)
/profile              → My progress, attempt history
/admin                → Admin dashboard (problem CRUD, user mgmt, attempts)
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
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      - DATABASE_URL=postgres://k8squiz:k8squiz@db:5432/k8squiz?sslmode=disable
      - GITHUB_CLIENT_ID=${GITHUB_CLIENT_ID}
      - GITHUB_CLIENT_SECRET=${GITHUB_CLIENT_SECRET}
      - JWT_SECRET=${JWT_SECRET}
      - JWT_REFRESH_SECRET=${JWT_REFRESH_SECRET}
      - PROBLEMS_REPO_PATH=/problems
      - FRONTEND_URL=http://localhost:5173
    depends_on:
      db:
        condition: service_healthy
    restart: unless-stopped

  frontend:
    build: ./frontend
    ports:
      - "5173:80"
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
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U k8squiz"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

volumes:
  pgdata:
```

## Environment Variables

```
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
JWT_SECRET=
JWT_REFRESH_SECRET=
DATABASE_URL=postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable
PROBLEMS_REPO_PATH=./problems
FRONTEND_URL=http://localhost:5173
DOCKER_HOST=unix:///var/run/docker.sock
SERVER_PORT=8080
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
- [ ] WebSocket hub (connection management)
- [ ] Terminal handler: auth → docker exec bridge
- [ ] xterm.js frontend component
- [ ] Stage progress messages during setup
- [ ] Resize handling

### Phase 5: Problems
- [ ] ProblemLoader interface + Git implementation
- [ ] Problem CRUD API (admin)
- [ ] Problem list/detail API (user)
- [ ] Problem sync endpoint (git pull)
- [ ] Frontend: dashboard, problem detail page

### Phase 6: Verification
- [ ] Verify endpoint (async, docker exec verify.sh)
- [ ] Choice submission endpoint
- [ ] WebSocket result push
- [ ] Attempt recording (status, duration, log)
- [ ] Frontend: verify button, result display

### Phase 7: Session Management
- [ ] Session start/reset/end
- [ ] 1 concurrent session per user enforcement
- [ ] Timeout warning + auto-end
- [ ] Server restart cleanup
- [ ] Frontend: session state, timer display

### Phase 8: Frontend Polish
- [ ] Profile page (attempt history, progress)
- [ ] Admin pages (problem CRUD, user list, attempts)
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

- Docker Socket: backend only creates/removes/execs containers with specific labels
- Container: resource limits enforced (1 CPU, 1GB RAM, 5GB disk)
- Network: each user container on isolated Docker network, no inter-container access
- JWT: short-lived access, hashed refresh tokens in DB
- Input: sanitize terminal input is NOT needed (it's a shell by design), but API inputs validated
- CORS: restrict to FRONTEND_URL

## Future (Not in v1)

- Leaderboard / achievements
- Text grading via LLM
- Docker image registry for problems (replace Git loader)
- Redis for WebSocket state (horizontal scaling)
- Multi-org / team support
- Deploy-it problem type
- Container pre-warm pool
