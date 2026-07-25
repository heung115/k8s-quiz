#!/bin/sh
# verify.sh — deploy-nginx. Grade = exit code (0 == solved).
# Assert the OUTCOME: Deployment web-server exists with spec.replicas==3 AND
# status.readyReplicas==3 (all pods Ready) AND container image == nginx:alpine
# (the problem requirement). Polling lets the 3 replicas converge and the image
# pull. Hard ~70s wall-clock deadline under the backend's 90s exec budget.
# Fail closed.
set -u

DEADLINE=$(( $(date +%s) + 70 ))
n=0
while [ "$n" -lt 23 ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
  REPLICAS=$(kubectl get deployment web-server -o jsonpath='{.spec.replicas}' 2>/dev/null)
  READY=$(kubectl get deployment web-server -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
  IMAGE=$(kubectl get deployment web-server -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
  if [ "$REPLICAS" = "3" ] && [ "$READY" = "3" ] && [ "$IMAGE" = "nginx:alpine" ]; then
    echo "SUCCESS: web-server deployed with 3 ready replicas using nginx:alpine"
    exit 0
  fi
  n=$((n + 1))
  sleep 3
done

echo "FAIL: web-server not satisfied within 70s (spec.replicas=${REPLICAS:-none} ready=${READY:-none} image=${IMAGE:-none}; want replicas=3 ready=3 image=nginx:alpine)"
exit 1
