#!/bin/sh
set -e
# bitnami/kubectl is pinned by immutable manifest digest (multi-arch index of
# the current release). Bitnami no longer publishes version tags (e.g. :1.31)
# on Docker Hub free tier, so a digest pin is the reproducible alternative to
# the floating :latest. Verified present on Docker Hub.
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
        image: bitnami/kubectl@sha256:95de17e6eb92da83a58c90a9df0c4cede634f898d2e6b92ea04f4a6ee6ace08d
        command: ["sleep", "3600"]
YAML
sleep 10
