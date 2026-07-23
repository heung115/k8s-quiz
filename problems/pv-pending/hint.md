# PVC Pending 힌트

1. `kubectl describe pvc data-pvc`로 이벤트를 확인하세요.
2. PVC가 요청하는 StorageClass가 실제로 존재하는지 확인하세요.
3. `kubectl get storageclass`로 사용 가능한 StorageClass를 볼 수 있습니다.
4. k3s에는 기본 StorageClass가 `local-path`로 제공됩니다.
5. PVC의 storageClassName을 올바른 값으로 수정하거나, PV를 직접 생성하는 방법을 고려하세요.
