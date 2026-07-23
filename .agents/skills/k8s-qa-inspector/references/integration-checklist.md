# K8s Quiz Integration Checklist

Compare both sides of each boundary. Cite the exact files compared. Mark each
`pass` / `fix` / `redo`.

## 1. REST contract ↔ Frontend types
- [ ] every response shape in `_workspace/03_backend_api_contract.md` matches the consuming TS type
- [ ] wrapped collections (`{"problems":[...]}`) are unwrapped consistently in the client
- [ ] field naming matches across layers (snake_case from Go vs the TS type)
- [ ] `correct_choice` is NOT present in user-facing problem responses
- [ ] `{error}` shape is handled by the client error path

## 2. WebSocket protocol ↔ Terminal client
- [ ] client sends `auth` first; server rejects non-`auth` first messages
- [ ] `input`/`output`/`resize` fields match on both sides
- [ ] `stage` values (`container_created|k3s_booting|setup_running|ready`) are all handled by the UI
- [ ] `verify_result`, `timeout_warning`, `session_ended`, `error` are all handled (no dropped frames)

## 3. verify.sh ↔ Grading
- [ ] backend grades by `verify.sh` exit code (0 == success) for `script` problems
- [ ] on the freshly-broken state, `verify.sh` exits non-zero (no false pass)
- [ ] after a correct fix, `verify.sh` exits 0 within its poll window (no false fail)
- [ ] verify output is captured into the attempt `verify_log`

## 4. problem.yaml ↔ Loader/parser
- [ ] every field the loader parses matches the schema the author wrote
- [ ] choice problems have ≥2 choices and a `correct_choice`; the parser rejects otherwise
- [ ] `hint.md` is loaded from the sibling file, not YAML
- [ ] `base_image` vs `image` resolves via `EffectiveImage()` consistently

## 5. Manager interface ↔ Backend usage
- [ ] backend calls only methods present in `_workspace/03_container_manager_iface.md`
- [ ] `Create` opts (labels, limits, network, problem mount) are populated by the session service
- [ ] `ExecInteractive` stream is bridged to the WS terminal and closed on disconnect

## 6. Routes ↔ Links / State flow
- [ ] every frontend navigation target resolves to a real route (incl. `/login/callback`)
- [ ] attempt status transitions (`in_progress → success|failed|timeout`) are actually performed in code, not just documented
- [ ] 1-session-per-user and timeout transitions are exercised

## Report Format
For each finding: boundary, specific mismatch, likely impact, smallest fix path,
owner. Then a summary: blocking issues vs follow-ups.
