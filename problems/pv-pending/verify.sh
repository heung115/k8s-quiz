#!/bin/sh
PVC_PHASE=$(kubectl get pvc data-pvc -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$PVC_PHASE" != "Bound" ]; then
  echo "FAIL: PVC phase=$PVC_PHASE (expected Bound)"
  exit 1
fi

POD_STATUS=$(kubectl get pods -l app=data-app -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
POD_READY=$(kubectl get pods -l app=data-app -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
if [ "$POD_STATUS" = "Running" ] && [ "$POD_READY" = "True" ]; then
  echo "SUCCESS: PVC is Bound and Pod is Running"
  exit 0
else
  echo "FAIL: Pod status=$POD_STATUS ready=$POD_READY"
  exit 1
fi
