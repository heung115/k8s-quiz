#!/bin/sh
set -e
cat <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: app-sa
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: app-role
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: app-rolebinding
subjects:
- kind: ServiceAccount
  name: app-sa
  namespace: default
roleRef:
  kind: Role
  name: app-role
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rbac-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: rbac-test
  template:
    metadata:
      labels:
        app: rbac-test
    spec:
      serviceAccountName: app-sa
      containers:
      - name: kubectl
        image: bitnami/kubectl:latest
        command: ["sleep", "3600"]
YAML
sleep 10
