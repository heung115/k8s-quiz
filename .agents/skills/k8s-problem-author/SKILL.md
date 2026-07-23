---
name: k8s-problem-author
description: Author k8s-quiz problems (problem.yaml, setup.sh that creates a broken k3s state, verify.sh that grades the fix, and hint.md) with verification scripts that cannot false-pass or false-fail.
---

# K8s Problem Author

Authors troubleshooting problems for k8s-quiz. Owns `problems/<id>/` exclusively.
The pedagogical and grading integrity of a problem lives here.

## When to Use
- use to create or revise a problem: its metadata, broken state, grading script, and hints
- use when a problem produces wrong pass/fail results (the verify script is the usual cause)
- do not use for backend, frontend, container, or QA work

## Required Inputs
- `PLAN.md` (`problem.yaml` schema, categories, difficulties, verify types)
- the target concept and the broken state to create
- the k3s-base image contract (`_workspace/03_container_manager_iface.md`)

## Workflow
1. Read `references/problem-schema.md` for the exact `problem.yaml` fields and constraints.
2. Read `references/verification-design.md` before writing `verify.sh` — grading integrity is the whole game.
3. Copy `templates/` into `problems/<id>/` and fill in: `problem.yaml`, `setup.sh`, `verify.sh`, optional `hint.md`.
4. Design the broken state in `setup.sh` (idempotent, exits 0 on success) and the success criterion in `verify.sh` (exit 0 iff truly solved).
5. Self-test the pair mentally against both a correct and an incorrect fix.
6. Emit the problem spec + expected verify behavior to `_workspace/03_problem_<id>.md` for QA.

## Outputs
- `problems/<id>/problem.yaml`
- `problems/<id>/setup.sh`
- `problems/<id>/verify.sh`
- `problems/<id>/hint.md` (optional, progressive)
- `_workspace/03_problem_<id>.md`

## Validation
- `problem.yaml` passes the loader's validation (title required; choice problems have ≥2 choices and a `correct_choice`)
- `verify.sh` exits non-zero on the freshly-broken state (no false pass) and 0 only after a real fix (no false fail)
- `setup.sh` is idempotent and exits 0; it does not depend on prior state
- `hint.md` never reveals the full answer up front

## References
- `references/problem-schema.md` — `problem.yaml` fields, enums, and validation rules
- `references/verification-design.md` — how to write verify scripts that grade correctly
- `templates/problem.yaml`, `templates/setup.sh`, `templates/verify.sh` — starting points
