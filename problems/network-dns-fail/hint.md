# DNS 해석 실패 힌트

1. `kubectl get pods -n kube-system`으로 시스템 Pod 상태를 확인하세요.
2. CoreDNS Pod이 정상적으로 실행 중인지 확인하세요.
3. DNS 서비스는 kube-system 네임스페이스에서 동작합니다.
4. CoreDNS의 replica 수를 확인하고, 0이라면 스케일 업이 필요합니다.
5. `kubectl get deployment coredns -n kube-system`으로 현재 상태를 파악하세요.
