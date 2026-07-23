#!/bin/sh
POD=$(kubectl get pods -l app=rbac-test -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$POD" ]; then
  echo "FAIL: rbac-test pod not found"
  exit 1
fi

RESULT=$(kubectl exec "$POD" -- kubectl auth can-i list pods --namespace=default 2>/dev/null)
if [ "$RESULT" = "yes" ]; then
  ACTUAL=$(kubectl exec "$POD" -- kubectl get pods --namespace=default -o name 2>/dev/null)
  if [ $? -eq 0 ]; then
    echo "SUCCESS: app-sa can list pods in default namespace"
    exit 0
  fi
  echo "FAIL: auth can-i says yes but actual list failed"
  exit 1
else
  echo "FAIL: app-sa cannot list pods (can-i=$RESULT)"
  exit 1
fi
