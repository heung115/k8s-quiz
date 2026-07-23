# _workspace/

Deterministic coordination artifacts for the k8s-quiz harness. Names are
predictable so any agent can resume or audit a run without rereading the skills.

```text
_workspace/
├── 00_input/
│   └── request-summary.md          # frozen request + assumptions (orchestrator)
├── 03_backend_api_contract.md      # backend → QA: real routes/response shapes
├── 03_frontend_consumers.md        # frontend → QA: hooks/types consuming API+WS
├── 03_container_manager_iface.md   # container → backend: final Manager interface
├── 03_problem_<id>.md              # problem author → QA: per-problem spec
├── 04_review_integration.md        # QA → orchestrator: boundary mismatches
└── final/
    └── integration-summary.md      # accepted deliverable + how to run
```

Phase prefix convention: `00` input, `01–02` contracts, `03` fan-out build,
`04` review, `final` accepted output. Low-risk ephemeral coordination may return
a bounded summary instead of a file; anything needing audit/resume lives here.
