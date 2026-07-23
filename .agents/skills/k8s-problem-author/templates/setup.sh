#!/bin/sh
# setup.sh — create the BROKEN state. Must be idempotent and exit 0 on success.
# Runs inside the container with the problem dir mounted read-only at /problem.
set -eu

# Example: deploy an app whose ConfigMap is missing, causing CrashLoopBackOff.
kubectl apply -f /problem/manifests/broken.yaml

echo "broken state created"
exit 0
