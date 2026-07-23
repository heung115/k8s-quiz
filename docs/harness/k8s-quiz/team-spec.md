# K8s Quiz — Harness Team Spec

> Durable role topology, handoff contract, and failure policy for building and
> maintaining the k8s-quiz platform. Generated with the Harness meta-skill from
> `PLAN.md`. Keep this file authoritative for *who owns what*; keep day-to-day
> coordination artifacts in `_workspace/`.

## Domain Analysis

**Product.** A web-based Kubernetes troubleshooting platform: a user starts a
problem, receives a broken k3s environment in a per-user container, fixes it
through a web terminal (xterm.js over WebSocket), and is graded by a
verification script or multiple-choice answer.

**Source of truth.** `PLAN.md` (tech stack, API routes, DB schema, WebSocket
protocol, container lifecycle, implementation phases) and `AGENTS.md`
(repo-wide WHAT/WHY/HOW). This harness does not duplicate them; it organizes the
*work* of building to that spec.

**Work characteristics that drive the design:**

- The stack splits into largely independent surfaces (Go backend, React
  frontend, Docker/k3s infra, quiz content) that can progress in parallel with
  non-overlapping file ownership.
- Integration failures cluster at *boundaries* (REST/WS contract ↔ frontend
  types, `verify.sh` exit code ↔ grading, `problem.yaml` ↔ loader parser), so a
  dedicated reviewer that reads both sides of each boundary is high value.
- Quiz content (`problem.yaml` + `setup.sh` + `verify.sh`) is pedagogically
  sensitive: a broken `verify.sh` produces false pass/fail, so it deserves a
  specialist and explicit validation.

## Architecture Pattern

**Fan-out/Fan-in + Producer-Reviewer** (outer pattern documented first).

- *Fan-out/Fan-in*: backend, frontend, container, and problem-authoring
  specialists work on independent slices, then the orchestrator synthesizes an
  integrated, runnable stack.
- *Producer-Reviewer*: every produced slice passes the QA inspector, which
  checks boundary coherence (not mere presence) before integration is accepted.

**Why not a pure Pipeline.** PLAN.md's phases look sequential, but the expensive
work (backend vs frontend vs infra vs content) is parallelizable; forcing it
sequential wastes the main benefit of delegation. The pipeline ordering is
preserved *within* the integration phase, where later stages genuinely depend on
earlier ones (scaffold → auth → container → ws → problems → verification).

**Delegation decision gate (recorded):**

- Independent units: backend packages, frontend pages/stores, container manager
  + k3s image, individual problems.
- Value source: specialization (k3s/Docker and quiz pedagogy are niche) +
  parallel latency + context isolation (each surface has a large surface area).
- Owned paths: see File Ownership below — strictly non-overlapping.
- Synthesis owner: the orchestrator (single owner) integrates and accepts.
- Partial failure: a failed slice is reported to `_workspace/` and does not
  block independent slices; integration proceeds with what is green.

## Roles

| Role | Responsibility | Reusable skill | Primary writes |
| --- | --- | --- | --- |
| Orchestrator | Phase order, fan-out, synthesis, acceptance | `.agents/skills/k8s-quiz-orchestrator/SKILL.md` | `_workspace/final/*` |
| Backend Engineer | Go/Gin API, Postgres, auth, WS hub, session/problem services | `.agents/skills/k8s-backend-engineer/SKILL.md` | `backend/` (except `internal/container/`) |
| Frontend Engineer | React/Vite/xterm UI, stores, API+WS clients | `.agents/skills/k8s-frontend-engineer/SKILL.md` | `frontend/` |
| Container Engineer | `ContainerManager`, Docker impl, k3s-base image, lifecycle | `.agents/skills/k8s-container-engineer/SKILL.md` | `backend/internal/container/`, `docker/` |
| Problem Author | `problem.yaml`, `setup.sh`, `verify.sh`, `hint.md` per problem | `.agents/skills/k8s-problem-author/SKILL.md` | `problems/<id>/` |
| QA Inspector | Cross-boundary coherence review (read-only) | `.agents/skills/k8s-qa-inspector/SKILL.md` | `_workspace/*_review_*.md` |

## File Ownership (non-overlapping)

Strict ownership is what makes parallel writes safe. No two writers touch the
same path.

| Owner | Owns | Must not touch |
| --- | --- | --- |
| Backend Engineer | `backend/cmd/`, `backend/pkg/`, `backend/migrations/`, `backend/internal/{auth,user,problem,session,ws}/` | `backend/internal/container/`, `frontend/`, `problems/`, `docker/` |
| Frontend Engineer | `frontend/` | `backend/`, `problems/`, `docker/` |
| Container Engineer | `backend/internal/container/`, `docker/k3s-base/` | other `backend/internal/*`, `frontend/`, `problems/` |
| Problem Author | `problems/<id>/` | `backend/`, `frontend/`, `docker/` |
| QA Inspector | `_workspace/*_review_*.md` (read-only otherwise) | all source paths |
| Orchestrator | `_workspace/final/`, root integration files (`docker-compose.yaml`, `.github/`, `README.md`) | delegates source edits to owners |

Shared *contracts* (REST routes, WS message protocol, DB schema, `problem.yaml`
schema) live in `PLAN.md`. Changing a contract is an orchestrator-level action
that notifies affected owners; specialists do not unilaterally change contracts.

## Phase Order

1. **Snapshot** — orchestrator records the request + assumptions to
   `_workspace/00_input/request-summary.md`.
2. **Contracts** — confirm the slice of `PLAN.md` in scope (routes, schema, WS
   protocol, problem schema). No code yet.
3. **Fan-out build** — backend, frontend, container, problem-author work in
   parallel on their owned paths.
4. **Boundary review** — QA inspector reads both sides of each boundary, writes
   `_workspace/0N_review_*.md`.
5. **Integration** — orchestrator wires `docker-compose.yaml`, env, migrations
   order; resolves review findings; produces a runnable stack.
6. **Validation** — build + unit tests green; one normal-flow and one
   failure-flow scenario exercised; final summary in `_workspace/final/`.

## Handoff Files

| From | To | File | Purpose |
| --- | --- | --- | --- |
| Orchestrator | all | `_workspace/00_input/request-summary.md` | frozen request + assumptions |
| Backend | QA | `_workspace/03_backend_api_contract.md` | actual routes/response shapes emitted |
| Frontend | QA | `_workspace/03_frontend_consumers.md` | hooks/types that consume the API + WS |
| Container | Backend | `_workspace/03_container_manager_iface.md` | final `Manager` interface + lifecycle notes |
| Problem Author | QA | `_workspace/03_problem_<id>.md` | problem spec + expected verify behavior |
| QA | Orchestrator | `_workspace/04_review_integration.md` | boundary mismatches + fix paths |
| Orchestrator | — | `_workspace/final/integration-summary.md` | accepted deliverable + how to run |

## Failure Policy

- **Retry policy.** A specialist that fails its slice reports the blocker to its
  handoff file and stops; the orchestrator may reassign or reduce scope rather
  than loop silently.
- **Partial completion.** Independent slices proceed; integration accepts the
  green subset and lists deferred slices explicitly in the final summary.
- **Conflict resolution.** Contract conflicts are resolved by the orchestrator
  in favor of `PLAN.md`; the losing side adapts. Never reconcile uncontrolled
  parallel-write interference after the fact — ownership prevents it.
- **Escalation trigger.** Escalate to the user when a contract in `PLAN.md` is
  ambiguous or self-contradictory, or when a slice needs a permission/tool the
  harness lacks (e.g. a running Docker daemon for live k3s verification).

## Removable Model-Specific Logic

Kept out of this spec by design. Any temporary retry/heuristic a specialist
needs (e.g. "retry `docker pull` twice on timeout") belongs in that skill's
`references/` under a clearly named removable section, with a deletion trigger
("remove once the image cache is warmed"). This spec stays runtime-neutral.

## Validation Checks

- **Structural.** Every generated `SKILL.md` has YAML frontmatter with `name`
  and `description`; referenced `references/*` files exist; ownership paths do
  not overlap.
- **Content.** Each specialist's Outputs section names deterministic artifacts;
  the QA checklist covers every boundary listed in `PLAN.md`.
- **Scenario.** One normal flow (build a problem end-to-end) and one failure
  flow (a `verify.sh` that exits non-zero) are described and exercised.

See `.agents/skills/k8s-quiz-orchestrator/SKILL.md` for the executable workflow.
