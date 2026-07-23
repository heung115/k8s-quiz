#!/bin/sh
# verify.sh — grade the fix. Exit 0 iff truly solved; fail closed otherwise.
# Observe only; never mutate the cluster. Print a short reason to stdout.
set -u

for _ in $(seq 1 30); do
  phase=$(kubectl get pod -l app=__APP_LABEL__ -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
  ready=$(kubectl get pod -l app=__APP_LABEL__ -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$ready" = "true" ]; then
    echo "Pod is Running and Ready. Solved."
    exit 0
  fi
  sleep 2
done

echo "Not solved yet (phase=${phase:-unknown} ready=${ready:-unknown})."
exit 1
