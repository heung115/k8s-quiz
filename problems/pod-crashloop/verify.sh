#!/bin/sh
# verify.sh — pod-crashloop. Grade = exit code (0 == solved).
# Assert the OUTCOME: any app=web-app pod is Running AND Ready.
# Rollout-safe: scan ALL matching pods (a fresh fixed pod may coexist with the
# old broken one mid-rollout, so never trust items[0]). Bounded by a hard
# ~70s wall-clock deadline (under the backend's 90s exec budget). Fail closed.
set -u

LABEL="app=web-app"
DEADLINE=$(( $(date +%s) + 70 ))
n=0
while [ "$n" -lt 23 ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
  PODS=$(kubectl get pods -l "$LABEL" -o jsonpath='{range .items[*]}{.status.phase}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)
  if printf '%s\n' "$PODS" | grep -q '^Running True$'; then
    echo "SUCCESS: an app=web-app pod is Running and Ready"
    exit 0
  fi
  n=$((n + 1))
  sleep 3
done

echo "FAIL: no app=web-app pod is Running+Ready within 70s (last state: ${PODS:-none})"
exit 1
