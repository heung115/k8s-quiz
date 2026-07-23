# 웹 서버 배포 힌트

1. `kubectl create deployment web-server --image=nginx:alpine`으로 빠르게 시작할 수 있습니다.
2. replica 수는 `kubectl scale deployment web-server --replicas=3` 또는 매니페스트의 `spec.replicas`로 설정합니다.
3. `kubectl get deployment web-server`로 원하는 replica 수와 ready 수를 확인하세요.
4. `kubectl get pods -l app=web-server`로 모든 Pod가 Running인지 확인하세요.
