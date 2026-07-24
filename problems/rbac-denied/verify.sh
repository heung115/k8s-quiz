#!/bin/sh
# verify.sh — rbac-denied. Grade = exit code (0 == solved).
# Assert the OUTCOME: from inside the app-sa helper pod (app=rbac-test,
# bitnami/kubectl), app-sa can list pods in the default namespace.
# Poll until a Ready helper pod exists (rollout-safe: scan all items, never
# items[0]), THEN exec the authorization check inside it. An exec failure or
# a denied result is a FAIL (fail closed). Hard ~70s wall-clock deadline
# (bounds the per-iteration exec cost) under the 90s exec budget.
set -u

LABEL="app=rbac-test"
DEADLINE=$(( $(date +%s) + 70 ))
LAST="no Ready rbac-test pod yet"
n=0
while [ "$n" -lt 23 ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
  POD=$(kubectl get pods -l "$LABEL" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep '^True ' | head -n 1 | cut -d' ' -f2)
  if [ -n "$POD" ]; then
    CANI=$(kubectl exec "$POD" -- kubectl auth can-i list pods --namespace=default 2>/dev/null)
    if [ "$CANI" = "yes" ] && kubectl exec "$POD" -- kubectl get pods --namespace=default -o name >/dev/null 2>&1; then
      echo "SUCCESS: app-sa can list pods in default namespace"
      exit 0
    fi
    LAST="pod=$POD can-i=${CANI:-error} (list denied or exec failed)"
  else
    LAST="no Ready rbac-test pod yet"
  fi
  n=$((n + 1))
  sleep 3
done

echo "FAIL: app-sa cannot list pods in default within 70s (${LAST})"
exit 1
