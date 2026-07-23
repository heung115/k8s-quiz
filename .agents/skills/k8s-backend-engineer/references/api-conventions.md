# Backend API Conventions

## Response Envelope
- Collections wrap items: `{"problems": [...]}`, `{"users": [...]}`, `{"attempts": [...]}`.
- Single resources return the object directly.
- Action results return `{"status": "..."}` or a typed result (e.g. verify returns `{success, message, log}`).

## Error Format
All errors are `{"error": "<message>"}` with an appropriate status:
- `400` validation/binding failure
- `401` missing/invalid token
- `403` non-admin on admin route
- `404` resource not found
- `409` no active session / state conflict
- `500` unexpected failure

## Route Map (from PLAN.md)
```
# Auth (public)
GET  /api/auth/github
GET  /api/auth/github/callback
POST /api/auth/refresh
POST /api/auth/logout

# User (auth)
GET  /api/users/me

# Problems (auth)
GET  /api/problems
GET  /api/problems/:id
POST /api/problems/:id/start
POST /api/problems/:id/verify
POST /api/problems/:id/submit-choice

# Session (auth)
GET  /api/sessions/active
POST /api/sessions/reset
POST /api/sessions/end
GET  /api/attempts

# Admin (auth + admin)
GET/POST       /api/admin/problems
PUT/DELETE     /api/admin/problems/:id
POST           /api/admin/problems/sync
GET            /api/admin/users
PUT            /api/admin/users/:id/role
GET            /api/admin/attempts
```

## Auth Rules
- JWT middleware on all `/api/*` except `/api/auth/*`.
- Admin routes additionally require `RequireAdmin()`.
- Never leak `correct_choice` on user-facing problem responses.

## Domain Layout
Each domain package: `model.go`, `repository.go` (interface + pgx impl),
`service.go`, `handler.go`. Services depend on interfaces; `main.go` wires
concrete implementations.
