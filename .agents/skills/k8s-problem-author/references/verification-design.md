# Verification Design

`verify.sh` exit code is the grade: **0 == solved, non-zero == not solved.** A
bad verify script is worse than no problem — it teaches the wrong lesson.

## The Two Sins
- **False pass:** exits 0 on the still-broken state. The user "wins" without fixing anything.
- **False fail:** exits non-zero after a correct fix (e.g. checks an exact string that varies). The user is punished for being right.

## Design Rules
1. **Assert the outcome, not the method.** Check that the Pod is `Running` and
   `Ready`, not that the user ran a specific command. Multiple correct fixes exist.
2. **Be robust to variation.** Use `kubectl` with jsonpath/json queries and
   tolerate whitespace/order. Avoid brittle exact-string matches on free text.
3. **Wait for convergence.** After a fix, controllers need time. Poll with a
   bounded timeout (e.g. up to 60s) before declaring failure.
4. **Fail closed.** Any unexpected error (missing resource, kubectl failure)
   must exit non-zero, never 0.
5. **No side effects.** verify must not mutate the cluster; it only observes.
6. **Print why.** Echo a short human-readable reason to stdout so the log helps
   the user (captured by the backend and shown on failure).

## Self-Test Checklist (before submitting)
- [ ] On the freshly-broken state, `verify.sh` exits non-zero.
- [ ] After the intended fix, `verify.sh` exits 0 within the poll window.
- [ ] After a *plausible but wrong* fix, `verify.sh` still exits non-zero.
- [ ] A kubectl error path exits non-zero (fail closed).

## Example (script verify)
```sh
#!/bin/sh
# Pass only when the target pod is Running and all containers Ready.
for i in $(seq 1 30); do
  phase=$(kubectl get pod -l app=web -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
  ready=$(kubectl get pod -l app=web -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$ready" = "true" ]; then
    echo "Pod is Running and Ready. Solved."
    exit 0
  fi
  sleep 2
done
echo "Pod is not healthy yet (phase=$phase ready=$ready)."
exit 1
```
