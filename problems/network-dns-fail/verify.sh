#!/bin/sh
# verify.sh — network-dns-fail. Grade = exit code (0 == solved).
# Assert the OUTCOME the problem promises: in-cluster DNS resolves the
# Service defined by setup.sh (my-service.default.svc.cluster.local).
# Ensure a known-good debug pod with nslookup exists (recreate if missing),
# wait for it to be Ready, then poll nslookup through it until it resolves
# (covers CoreDNS scale-up). Cleans up its debug pod on exit (trap). Hard
# ~70s wall-clock deadline under the backend's 90s exec budget. Fail closed.
set -u

TARGET="my-service.default.svc.cluster.local"
DEBUG="verify-dns-debug"
DEADLINE=$(( $(date +%s) + 70 ))

cleanup() {
  kubectl delete pod "$DEBUG" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1
}
trap cleanup EXIT

# (Re)create a known-good busybox debug pod (nslookup available). Remove any
# stale instance first so apply cannot collide with a terminating pod.
cleanup
cat <<YAML | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: $DEBUG
  labels:
    app: verify-dns-debug
spec:
  restartPolicy: Never
  containers:
  - name: debug
    image: busybox:1.36
    command: ["sleep", "3600"]
YAML

READY=""
n=0
while [ "$n" -lt 23 ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
  READY=$(kubectl get pod "$DEBUG" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$READY" = "True" ] && kubectl exec "$DEBUG" -- nslookup "$TARGET" >/dev/null 2>&1; then
    echo "SUCCESS: in-cluster DNS resolves $TARGET"
    exit 0
  fi
  n=$((n + 1))
  sleep 3
done

echo "FAIL: $TARGET did not resolve via in-cluster DNS within 70s (debug pod ready=${READY:-unknown})"
exit 1
