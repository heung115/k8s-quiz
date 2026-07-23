# problem.yaml Schema

```yaml
id: pod-crashloop                 # unique, matches the directory name
title: "CrashLoopBackOff 해결"
description: |
  Pod이 CrashLoopBackOff 상태입니다. 원인을 파악하고 수정하세요.
category: pod                     # pod | network | storage | rbac | scheduling | config
difficulty: easy                  # easy | medium | hard
type: fix                         # fix | find
timeout_minutes: 30
verify_type: script               # script | choice
base_image: k3s-base:latest       # OR use `image:` for a custom image
# image: custom-problem:v1        # alternative to base_image
choices:                          # only for verify_type: choice
  - { id: a, text: "OOMKilled" }
  - { id: b, text: "ImagePullBackOff" }
  - { id: c, text: "CrashLoopBackOff due to missing ConfigMap" }
  - { id: d, text: "Node NotReady" }
correct_choice: c                 # only for verify_type: choice
```

## Validation Rules (enforced by the loader)
- `title` is required.
- `verify_type` must be `script` or `choice` (empty defaults to `script`).
- For `choice`: `correct_choice` is required and there must be ≥2 `choices`.
- `hint.md` is loaded from a sibling file if present (not from YAML).

## Conventions
- `id` == directory name; keep it kebab-case and stable (it is the DB primary key).
- Prefer `base_image: k3s-base:latest`; use a custom `image` only when the problem needs tools/state the base lacks.
- `type: find` problems still grade via `verify.sh` (e.g. assert the user created the right diagnostic artifact) or via `choice`.
