#!/bin/sh
set -e
cat <<'YAML' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-service
  template:
    metadata:
      labels:
        app: my-service
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
---
apiVersion: v1
kind: Service
metadata:
  name: my-service
spec:
  selector:
    app: my-service
  ports:
  - port: 80
    targetPort: 80
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: debug-pod
spec:
  replicas: 1
  selector:
    matchLabels:
      app: debug
  template:
    metadata:
      labels:
        app: debug
    spec:
      containers:
      - name: debug
        image: curlimages/curl:8.11.1
        command: ["sleep", "3600"]
YAML

# Wait (bounded) for the coredns deployment to exist and roll out before
# scaling it to 0. A blind `sleep 10` could race the k3s DNS addon on a fresh
# cluster, making `kubectl scale` hit NotFound and abort obscurely under
# set -e. If coredns is not ready within the budget, fail setup explicitly
# with a clear message instead of falling through to the scale.
WAIT=70
DEADLINE=$(( $(date +%s) + WAIT ))
COREDNS_READY=0
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  if kubectl -n kube-system rollout status deploy/coredns --timeout=5s >/dev/null 2>&1; then
    COREDNS_READY=1
    break
  fi
  sleep 2
done
if [ "$COREDNS_READY" -ne 1 ]; then
  echo "setup failed: coredns deployment did not become ready within ${WAIT}s" >&2
  exit 1
fi
# Broken end-state (unchanged): cluster DNS disabled, coredns at 0 replicas.
kubectl scale deployment coredns -n kube-system --replicas=0
