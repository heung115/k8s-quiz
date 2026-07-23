# K8s Quiz

Web-based Kubernetes troubleshooting platform. Users solve broken k3s environments via web terminal, verified by scripts.

## Stack

- Backend: Go + Gin (`backend/`)
- Frontend: React + Vite + TS + Tailwind + shadcn/ui (`frontend/`)
- DB: PostgreSQL (migrations in `backend/migrations/`)
- Infra: Docker Compose, Docker Socket mount, k3s-in-Docker
- Auth: GitHub OAuth → JWT (Access + Refresh)
- Realtime: WebSocket (terminal, stage progress, verify results)

## Key Commands

```bash
# Backend
cd backend && go run ./cmd/server
cd backend && go test ./...

# Frontend
cd frontend && npm install && npm run dev

# Full stack
docker compose up --build

# DB migrate
migrate -path backend/migrations -database "$DATABASE_URL" up
```

## Architecture

- Domain-based packages: `internal/{auth,user,problem,session,container,ws}/`
- Each domain: handler → service → repository
- `ContainerManager` interface abstracts Docker (future: K8s, remote)
- `ProblemLoader` interface abstracts problem source (Git now, image registry later)
- Problems live in a separate Git repo, loaded via clone/pull

## Conventions

- Go: standard layout, `internal/` for app code, `pkg/` for shared utils
- Frontend: pages in `src/pages/`, stores in `src/stores/`, API in `src/api/`
- Env vars for config (no viper), see `.env.example`
- REST for CRUD, WebSocket for terminal + async events
- JWT auth middleware on all `/api/*` except `/api/auth/*`

## Full Plan

See `PLAN.md` for complete spec: API routes, DB schema, WebSocket protocol, implementation phases.
