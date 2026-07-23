---
name: k8s-frontend-engineer
description: Implement the k8s-quiz React frontend (Vite + TS + Tailwind + shadcn/ui, xterm.js terminal over WebSocket, Zustand stores, API client) per the page and structure map in PLAN.md.
---

# K8s Frontend Engineer

Builds and maintains the React SPA for k8s-quiz. Owns `frontend/` exclusively.

## When to Use
- use for pages, components, the xterm terminal, stores, and the API/WebSocket clients
- use when adapting the UI to an orchestrator-approved contract change
- do not use for `backend/`, `docker/`, or `problems/`

## Required Inputs
- `PLAN.md` (pages, frontend structure) and the frozen request summary
- the backend's `_workspace/03_backend_api_contract.md` (real response shapes)
- the WS protocol (canonical spec owned by the backend skill; client handling in `references/xterm-websocket.md`)

## Workflow
1. Read `references/state-and-api.md` for store boundaries and the API client contract.
2. Scaffold pages under `src/pages/` matching PLAN.md routes: `/`, `/login`, `/problems/:id`, `/profile`, `/admin`, `/docs`.
3. Build the terminal as a reusable `src/components/Terminal/` wrapping xterm.js + fit addon, driven by the WS client (see `references/xterm-websocket.md`).
4. Type every API response from the backend contract; do not cast away shape mismatches — surface them to QA.
5. Emit consumed hooks/types and the WS message handlers to `_workspace/03_frontend_consumers.md`.

## Outputs
- `frontend/src/{api,components,pages,stores,hooks,types}/`
- `frontend/{vite.config.ts,tailwind.config.ts,tsconfig.json,package.json,Dockerfile}`
- `_workspace/03_frontend_consumers.md`

## Validation
- `npm run build` (and `tsc --noEmit`) are green with no `any`-cast shape hacks
- every navigation target resolves to a real route; links include required prefixes
- API types match `_workspace/03_backend_api_contract.md` field-for-field (QA compares both sides)

## References
- `references/state-and-api.md` — store boundaries, API client, auth token handling
- `references/xterm-websocket.md` — terminal ↔ WebSocket bridging and resize handling
