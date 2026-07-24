#!/bin/sh
# verify.sh — pv-pending. Grade = exit code (0 == solved).
# Assert the OUTCOME the problem promises: PVC data-pvc is Bound AND a
# consuming app=data-app pod is Running+Ready. Rollout-safe pod scan (never
# items[0]). The window covers PVC bind plus the nginx:alpine image pull.
# Hard ~70s wall-clock deadline under the backend's 90s exec budget. Fail closed.
set -u

LABEL="app=data-app"
DEADLINE=$(( $(date +%s) + 70 ))
n=0
while [ "$n" -lt 23 ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
  PVC_PHASE=$(kubectl get pvc data-pvc -o jsonpath='{.status.phase}' 2>/dev/null)
  PODS=$(kubectl get pods -l "$LABEL" -o jsonpath='{range .items[*]}{.status.phase}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)
  if [ "$PVC_PHASE" = "Bound" ] && printf '%s\n' "$PODS" | grep -q '^Running True$'; then
    echo "SUCCESS: PVC data-pvc is Bound and an app=data-app pod is Running+Ready"
    exit 0
  fi
  n=$((n + 1))
  sleep 3
done

echo "FAIL: not solved within 70s (pvc=${PVC_PHASE:-unknown} pods=${PODS:-none})"
exit 1
