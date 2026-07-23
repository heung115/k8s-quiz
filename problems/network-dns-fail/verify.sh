#!/bin/sh
POD=$(kubectl get pods -l app=debug -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$POD" ]; then
  echo "FAIL: debug pod not found"
  exit 1
fi

RESULT=$(kubectl exec "$POD" -- curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 http://my-service.default.svc.cluster.local 2>/dev/null)
if [ "$RESULT" = "200" ]; then
  echo "SUCCESS: DNS resolution and service access working"
  exit 0
else
  echo "FAIL: curl returned $RESULT"
  exit 1
fi
