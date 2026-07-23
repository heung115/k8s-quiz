# ConfigMap 키 오타 힌트

1. `kubectl describe pod -l app=web-app`로 Pod 이벤트를 확인하세요.
2. "configmap key not found" 같은 메시지를 찾아보세요.
3. `kubectl get configmap app-settings -o yaml`로 ConfigMap의 실제 키를 확인하세요.
4. Deployment에서 참조하는 key와 ConfigMap의 실제 key를 비교해보세요.
5. 환경 변수 정의에서 configMapKeyRef의 key 값에 주목하세요.
