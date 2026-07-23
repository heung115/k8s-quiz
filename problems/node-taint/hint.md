# Node Taint 힌트

1. `kubectl describe node`로 Node의 Taints 섹션을 확인하세요.
2. `kubectl get pods -l app=web-frontend -o wide`로 Pod이 어디에 스케줄링되는지 보세요.
3. `kubectl describe pod`의 Events에서 "had taint" 관련 메시지를 찾을 수 있습니다.
4. taint를 제거하거나, Pod에 toleration을 추가하는 두 가지 방법이 있습니다.
5. taint 제거: `kubectl taint nodes <node-name> dedicated:NoSchedule-`
