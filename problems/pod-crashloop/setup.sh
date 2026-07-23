#!/bin/sh
set -e
cat <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  APP_PORT: "8080"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web-app
  template:
    metadata:
      labels:
        app: web-app
    spec:
      containers:
      - name: web
        image: nginx:alpine
        env:
        - name: REQUIRED_CONFIG
          valueFrom:
            configMapKeyRef:
              name: app-config
              key: MISSING_KEY
YAML
sleep 5
