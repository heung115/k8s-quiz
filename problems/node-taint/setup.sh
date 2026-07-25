#!/bin/sh
set -e
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
# --overwrite makes the taint idempotent: re-running setup must not fail under
# set -e when the taint already exists on the node.
kubectl taint nodes "$NODE" dedicated=special:NoSchedule --overwrite

cat <<'YAML' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-frontend
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web-frontend
  template:
    metadata:
      labels:
        app: web-frontend
    spec:
      containers:
      - name: web
        image: nginx:alpine
        ports:
        - containerPort: 80
YAML
sleep 5
