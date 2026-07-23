#!/bin/sh
POD_STATUS=$(kubectl get pods -l app=web-frontend -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
POD_READY=$(kubectl get pods -l app=web-frontend -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)

if [ "$POD_STATUS" = "Running" ] && [ "$POD_READY" = "True" ]; then
  echo "SUCCESS: web-frontend Pod is Running and Ready"
  exit 0
else
  echo "FAIL: Pod status=$POD_STATUS ready=$POD_READY"
  exit 1
fi
