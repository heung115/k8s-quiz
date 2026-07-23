# RBAC 권한 거부 힌트

1. `kubectl get role app-role -o yaml`로 현재 Role의 규칙을 확인하세요.
2. Role에 pods 리소스에 대한 권한이 포함되어 있는지 살펴보세요.
3. `kubectl auth can-i list pods --as=system:serviceaccount:default:app-sa`로 권한을 테스트할 수 있습니다.
4. Role의 rules에 pods 리소스와 list verb를 추가해야 합니다.
5. RoleBinding이 올바른 Role과 ServiceAccount를 연결하는지도 확인하세요.
