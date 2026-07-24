# Local Load Test Baseline — 2026-07-24

## Purpose

K8s Quiz 백엔드의 로컬 기준선을 만들고, 같은 조건으로 반복 실행할 수
있는 부하 테스트를 준비한다.

이 결과는 로컬 Docker 환경의 단순 `/health` 엔드포인트를 측정한
것이다. 실제 사용자 흐름, 데이터베이스 쿼리, WebSocket 터미널,
k3s 컨테이너 생성 또는 프로덕션 용량을 나타내지 않는다.

## Environment

- Host: Apple Silicon macOS
- Backend: Docker Compose의 `k8s-quiz-backend-1`
- Target: `http://127.0.0.1:8080/health`
- Tool: k6
- Scenario: constant-arrival-rate
- Rate: 200 iterations/s
- Duration: 30s
- Maximum VUs: 100

## Command

```bash
k6 run \
  --summary-export=/tmp/k8s-quiz-health-baseline-2026-07-24.json \
  load-tests/health.js
```

## Result

| Metric | Result |
|---|---:|
| HTTP requests | 6,000 |
| Achieved rate | 199.996 req/s |
| Failed requests | 0 |
| Dropped iterations | 0 |
| Checks | 12,000 passed / 0 failed |
| Average response time | 1.285 ms |
| Median response time | 1.312 ms |
| p95 response time | 1.752 ms |
| Maximum response time | 6.1 ms |

Configured thresholds all passed:

- check success rate > 99%
- dropped iterations = 0
- p95 < 100 ms
- p99 < 250 ms
- HTTP failure rate < 1%

## Verification

The backend regression suite was run after the load test:

```bash
cd backend
go test ./...
```

All test packages passed on 2026-07-24.

## Next Measurement

인증된 문제 목록 조회, PostgreSQL이 포함된 진행률 조회, WebSocket 연결,
k3s 실습 환경 생성은 별도 시나리오로 분리해야 한다. 각 시나리오에는
테스트 데이터 준비, 자원 사용량, 병목과 실패 조건을 함께 기록한다.
