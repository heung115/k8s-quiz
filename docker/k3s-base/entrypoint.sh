#!/bin/sh
set -e

# Start k3s server in background
/bin/k3s server \
  --disable=traefik \
  --disable=metrics-server \
  --write-kubeconfig-mode=644 \
  --disable-network-policy &

# Wait for k3s to be ready
echo "Waiting for k3s to start..."
for i in $(seq 1 60); do
  if kubectl get nodes >/dev/null 2>&1; then
    echo "k3s is ready"
    break
  fi
  sleep 1
done

# Keep container running
exec tail -f /dev/null
