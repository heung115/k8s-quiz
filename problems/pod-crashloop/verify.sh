#!/bin/sh
STATUS=$(kubectl get pods -l app=web-app -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
READY=$(kubectl get pods -l app=web-app -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)

if [ "$STATUS" = "Running" ] && [ "$READY" = "True" ]; then
  echo "SUCCESS: Pod is Running and Ready"
  exit 0
else
  echo "FAIL: Pod status=$STATUS ready=$READY"
  exit 1
fi
