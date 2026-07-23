---
name: k8s-quiz-orchestrator
description: Coordinate the k8s-quiz platform build across backend, frontend, container, problem-authoring, and QA specialists with deterministic handoffs and boundary review.
---

# K8s Quiz Orchestrator

Top-level orchestration for building and maintaining the k8s-quiz platform per
`PLAN.md`. Fans out to specialists with non-overlapping ownership, runs a
boundary-coherence review, then synthesizes a runnable stack.

## When to Use
- use when building or extending the k8s-quiz platform end-to-end
- use when work spans backend + frontend + infra + quiz content and benefits from parallel specialists
- do not use for a single-surface tweak one specialist can finish alone

## Required Inputs
- `PLAN.md` (contract authority: routes, DB schema, WS protocol, problem schema)
- the request, frozen into `_workspace/00_input/request-summary.md`
- a running Docker daemon *only* if live container/k3s verification is requested

## Goal
Deliver a runnable k8s-quiz stack (backend + frontend + db + at least one
solvable problem) that satisfies the acceptance bar in the request summary, with
every boundary checked by QA before acceptance.

## Roles
| Role | Responsibility | Skill | Writes |
| --- | --- | --- | --- |
| Backend Engineer | API, DB, auth, WS hub, services | `.agents/skills/k8s-backend-engineer/SKILL.md` | `backend/` (except `internal/container/`) |
| Frontend Engineer | React UI, xterm, stores, clients | `.agents/skills/k8s-frontend-engineer/SKILL.md` | `frontend/` |
| Container Engineer | `Manager`, Docker impl, k3s-base | `.agents/skills/k8s-container-engineer/SKILL.md` | `backend/internal/container/`, `docker/` |
| Problem Author | problem.yaml/setup.sh/verify.sh | `.agents/skills/k8s-problem-author/SKILL.md` | `problems/<id>/` |
| QA Inspector | boundary-coherence review (read-only) | `.agents/skills/k8s-qa-inspector/SKILL.md` | `_workspace/*_review_*.md` |

## Phase Order

### Phase 1: Snapshot & Contracts
- input sources: request, `PLAN.md`
- actions: freeze `_workspace/00_input/request-summary.md`; confirm in-scope routes/schema/WS protocol/problem schema
- output files: `_workspace/00_input/request-summary.md`
- completion criteria: request frozen; contract slice enumerated; no code yet

### Phase 2: Fan-out Build
- input sources: frozen request, contracts
- actions: dispatch backend, frontend, container, problem-author in parallel on owned paths
- output files: `_workspace/03_backend_api_contract.md`, `_workspace/03_frontend_consumers.md`, `_workspace/03_container_manager_iface.md`, `_workspace/03_problem_<id>.md`
- completion criteria: each slice compiles/builds in isolation; handoff files written

### Phase 3: Boundary Review
- input sources: the `03_*` handoff files + source on both sides of each boundary
- actions: QA inspector compares both sides of every boundary
- output files: `_workspace/04_review_integration.md`
- completion criteria: each boundary marked pass/fix/redo with a smallest fix path

### Phase 4: Integration
- input sources: green slices, `04_review_integration.md`
- actions: orchestrator wires `docker-compose.yaml`, env, migration order; routes review findings to owners; re-runs affected slices
- output files: integrated tree, updated handoff files
- completion criteria: no `fix`/`redo` findings remain open; stack boots

### Phase 5: Validation
- input sources: integrated tree
- actions: `go build ./... && go test ./...`, `npm run build`, `docker compose up --build`; one normal-flow + one failure-flow scenario
- output files: `_workspace/final/integration-summary.md`
- completion criteria: acceptance bar from request summary met; summary written

## Handoff Files
| From | To | File | Purpose |
| --- | --- | --- | --- |
| Orchestrator | all | `_workspace/00_input/request-summary.md` | frozen request |
| Backend | QA | `_workspace/03_backend_api_contract.md` | real routes/response shapes |
| Frontend | QA | `_workspace/03_frontend_consumers.md` | consuming hooks/types |
| Container | Backend | `_workspace/03_container_manager_iface.md` | final `Manager` interface |
| Problem Author | QA | `_workspace/03_problem_<id>.md` | per-problem expected verify behavior |
| QA | Orchestrator | `_workspace/04_review_integration.md` | mismatches + fix paths |
| Orchestrator | — | `_workspace/final/integration-summary.md` | accepted deliverable + run steps |

## Failure Policy
- retry policy: a failed slice reports its blocker and stops; reassign or reduce scope, never loop silently
- partial completion: integrate the green subset; list deferred slices in the final summary
- conflict resolution: contract conflicts resolve in favor of `PLAN.md`; the losing side adapts
- escalation trigger: ambiguous/contradictory `PLAN.md`, or a slice needs a missing permission/tool (e.g. Docker daemon)

## Removable Model-Specific Logic
- temporary retries/heuristics live in each specialist's `references/`, not here
- deletion trigger: when the underlying capability is stable (e.g. image cache warmed), remove the heuristic

## Validation Checks
- structural: every `SKILL.md` has `name`+`description` frontmatter; referenced files exist; ownership non-overlapping
- content: each boundary in `PLAN.md` is covered by a QA check
- scenario: normal flow (solve a problem) + failure flow (`verify.sh` exits non-zero)

## Worker Delegation Notes
- eligible classes: backend packages, frontend pages/stores, container manager + k3s image, individual problems
- safe parallel slices: any set respecting File Ownership in `docs/harness/k8s-quiz/team-spec.md`
- forbidden overlaps: two writers on the same path; unilateral contract changes
- write ownership: see team-spec; QA is read-only on source
- concurrency/depth: one coordination layer; max depth 1; orchestrator owns synthesis
- result format: handoff file per slice, or a bounded summary for trivial slices
- synthesis owner: orchestrator; acceptance = request-summary bar met
- partial-worker failure: report + proceed with green subset
- conflicting-result resolution: defer to `PLAN.md`
- escalation trigger: as in Failure Policy

## Test Scenarios
### Normal flow
- request: "add problem `pod-crashloop` and make it solvable end-to-end"
- expected phase outputs: `03_problem_pod-crashloop.md`, green backend verify endpoint, terminal reaches container
- expected final output: `_workspace/final/integration-summary.md` with run steps; verify.sh exit 0 → success
### Failure flow
- failure point: `verify.sh` exits non-zero after the user's fix attempt
- expected fallback: attempt marked `failed`, log captured, user prompted to retry/reset
- expected reporting: finding recorded; no silent success

See `docs/harness/k8s-quiz/team-spec.md` for role topology and ownership.
