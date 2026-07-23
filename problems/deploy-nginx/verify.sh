#!/bin/sh
REPLICAS=$(kubectl get deployment web-server -o jsonpath='{.spec.replicas}' 2>/dev/null)
if [ "$REPLICAS" != "3" ]; then
  echo "FAIL: web-server replicas=$REPLICAS (expected 3)"
  exit 1
fi

READY=$(kubectl get deployment web-server -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
if [ "$READY" = "3" ]; then
  echo "SUCCESS: web-server deployed with 3 ready replicas"
  exit 0
fi
echo "FAIL: ready replicas=$READY (expected 3)"
exit 1
