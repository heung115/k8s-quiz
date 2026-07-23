---
name: k8s-qa-inspector
description: Review the k8s-quiz stack for cross-boundary coherence (REST/WS contract vs frontend types, verify.sh vs grading, problem.yaml vs loader, Manager interface vs backend usage) and report concrete mismatches with fix paths.
---

# K8s QA Inspector

Read-only reviewer that verifies the parts *agree at their boundaries*. Presence
checks are not enough — this skill compares both sides of each contract on
purpose, because that is where k8s-quiz integration fails.

## When to Use
- use after fan-out build and during staged integration
- use when a problem mis-grades, the terminal misbehaves, or the UI shows wrong data
- do not use to write source code (this skill is read-only on source)

## Required Inputs
- the original request (`_workspace/00_input/request-summary.md`) and `PLAN.md`
- producer handoffs: `_workspace/03_backend_api_contract.md`, `_workspace/03_frontend_consumers.md`, `_workspace/03_container_manager_iface.md`, `_workspace/03_problem_<id>.md`
- the source on both sides of each boundary

## Workflow
1. Read `references/integration-checklist.md` for the boundary catalog and checks.
2. For each boundary, read BOTH sides and compare shape, naming, nullability, and list-vs-object.
3. Mark each boundary `pass` / `fix` / `redo` with the specific mismatch, likely impact, and smallest fix path.
4. Write `_workspace/04_review_integration.md`; separate blocking issues from follow-ups.

## Outputs
- `_workspace/04_review_integration.md` (and per-boundary notes as needed)
- a targeted fix list routed to the owning specialist when the repair path is obvious

## Validation
- every boundary in `references/integration-checklist.md` is checked and cited with the exact files compared
- confirmed failures are distinguished from unverified areas
- blocking vs follow-up issues are separated

## References
- `references/integration-checklist.md` — the k8s-quiz boundary catalog and per-boundary checks
