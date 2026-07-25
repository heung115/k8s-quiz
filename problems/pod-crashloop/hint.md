# CreateContainerConfigError 힌트

1. `kubectl describe pod`로 Pod 이벤트를 확인하세요.
2. Events 섹션에서 컨테이너가 시작되지 못하는 이유를 찾아보세요.
3. ConfigMap에서 참조하는 키가 실제로 존재하는지 확인하세요.
4. `kubectl get configmap app-config -o yaml`로 ConfigMap 내용을 볼 수 있습니다.
5. Deployment의 env 설정에서 `configMapKeyRef`의 key 값을 다시 살펴보세요.
