# K8s Quiz

[![CI](https://github.com/heung115/k8s-quiz/actions/workflows/ci.yaml/badge.svg)](https://github.com/heung115/k8s-quiz/actions/workflows/ci.yaml)

Kubernetes 문제는 문서로 읽을 때보다, 실제로 한 번 망가뜨리고 고쳐볼 때 더 오래 남습니다. K8s Quiz는 브라우저에서 고장 난 k3s 환경에 접속해 `kubectl`로 원인을 찾고, 해결 결과를 검증받는 실습 플랫폼입니다.

현재 로컬 개발 경로에서는 문제를 고르면 사용자 전용 컨테이너가 만들어지고, 준비가 끝나면 웹 터미널이 열립니다. 문제를 해결한 뒤 **확인하기**를 누르면 검증 스크립트가 클러스터 상태를 판정합니다. 종료하거나 시간이 만료되면 컨테이너와 네트워크를 정리합니다. 이 privileged Docker 경로는 공개 서비스용 격리나 신뢰된 채점 경계가 아닙니다.

## 어떤 문제를 풀 수 있나요?

현재 일곱 가지 시나리오가 들어 있습니다.

| 영역 | 시나리오 |
| --- | --- |
| Pod | ConfigMap 키 오류로 인한 `CreateContainerConfigError` |
| Network | Service DNS 해석 실패 |
| Storage | PVC가 `Pending` 상태에 머무는 문제 |
| RBAC | ServiceAccount 권한 거부 |
| Scheduling | Node taint 때문에 스케줄링되지 않는 Pod |
| Config | ConfigMap 참조 오류 원인 찾기(객관식) |
| Deploy | nginx Deployment를 조건에 맞게 직접 배포하기 |

각 문제는 `problem.yaml`, `setup.sh`, `verify.sh`, `hint.md`로 정의됩니다. 문제를 추가하는 사람이 Kubernetes 상태를 만들고 검증하는 로직까지 함께 작성할 수 있도록 한 구조입니다.

## 구성

- **Backend** — Go, Gin, PostgreSQL, WebSocket
- **Frontend** — React, TypeScript, Vite, Tailwind CSS, Zustand, xterm.js
- **실습 환경** — Docker Compose와 k3s-in-Docker
- **인증** — GitHub OAuth, 짧은 수명의 access cookie와 refresh-token rotation

백엔드는 provider-neutral Runner 경계를 통해 실습 환경을 관리합니다. 현재 구현된 `local-docker` Runner는 신뢰할 수 있는 로컬 개발 전용입니다. REST API는 generation에 결합된 시작·리셋·종료 작업을 처리하고, `/ws/terminal`은 터미널 바이트만, PostgreSQL-backed `/ws/lifecycle`은 준비 단계·검증·정리 상태를 담당합니다. 터미널은 exact `terminal_attached` 확인 뒤에만 입력을 열며, 세션은 사용자당 하나만 유지하고 전역 동시 세션 상한을 설정할 수 있습니다.

## 시작하기

Docker Desktop 또는 Docker Engine이 실행 중이어야 합니다. 실습 컨테이너 안에서 k3s를 띄우므로 일반적인 프론트엔드 프로젝트보다 CPU와 메모리가 더 필요합니다.

예제 문제는 별도 저장소를 Git submodule로 참조합니다. 처음 clone할 때는 `--recurse-submodules`를 붙이거나, 이미 clone했다면 `make problems-init`을 한 번 실행하세요.

```bash
# 처음 clone하는 경우
git clone --recurse-submodules https://github.com/heung115/k8s-quiz.git
cd k8s-quiz

# 이미 clone한 경우에는 아래 명령만 실행
make problems-init

# 1. 로컬 설정을 만들고 GitHub OAuth 값을 채웁니다.
cp .env.example .env

# 2. 문제용 k3s 이미지를 먼저 빌드하고 앱·PostgreSQL을 실행합니다.
# make up은 내부에서 make image를 선행합니다.
make up
```

브라우저에서 [http://localhost:5173](http://localhost:5173)으로 접속합니다. 로그인까지 확인하려면 GitHub OAuth App의 callback URL을 아래처럼 등록해야 합니다.

```text
http://localhost:5173/api/auth/github/callback
```

`docker compose` 환경에서는 프론트엔드가 `/api`와 `/ws` 요청을 백엔드로 프록시합니다. 백엔드를 따로 실행할 때는 `.env`의 `DATABASE_URL`, `PROBLEMS_REPO_PATH`, `PROBLEM_ARTIFACT_STORE_PATH`를 현재 환경에 맞게 조정하세요. Artifact 경로는 mutable checkout과 겹치지 않는 지속 디렉터리여야 하며, DB와 함께 백업·복구해야 합니다.

## 로컬 개발

```bash
# PostgreSQL만 Docker로 실행하는 예시
docker run -d --name k8s-quiz-db \
  -e POSTGRES_USER=k8squiz \
  -e POSTGRES_PASSWORD=k8squiz \
  -e POSTGRES_DB=k8squiz \
  -p 5432:5432 postgres:16-alpine

# backend
cd backend && go run ./cmd/server

# 다른 터미널에서 frontend
cd frontend && npm install && npm run dev
```

마이그레이션은 서버가 시작될 때 적용됩니다. 별도로 실행하고 싶다면 다음 명령을 사용할 수 있습니다.

```bash
migrate -path backend/migrations \
  -database "postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable" up
```

## 검증

```bash
# backend: 단위/핸들러 테스트와 race detector
cd backend && go test ./... -short -race

# frontend: 타입 검사, 테스트, 프로덕션 빌드
cd frontend
npm ci
npx tsc --noEmit
npm test
npm run build

# 문제 스크립트 문법 검사 (shellcheck이 설치돼 있으면 함께 실행)
cd .. && make lint-problems
```

간단한 `/health` 부하 기준선과 PostgreSQL query-plan 재현 스크립트도 포함했습니다. 측정 방법과 결과는 [부하 테스트 기준선](docs/load-test-baseline-2026-07-24.md), [query-plan 기록](docs/postgres-query-plan-2026-07-24.md)에서 볼 수 있습니다.

## 문제를 추가하려면

공개 예제 문제와 문제 작성 규칙은 [`k8s-quiz-problems`](https://github.com/heung115/k8s-quiz-problems) 저장소에서 관리합니다. 문제를 추가하거나 수정할 때는 그 저장소의 README와 CI 검증을 먼저 확인한 뒤, 메인 저장소에서 `make problems-update`로 submodule 포인터를 갱신합니다.

문제 catalog의 generation·artifact·revision 보장처럼 애플리케이션과 연결된 동작은 [문제 artifact catalog ADR](docs/architecture/adr/002-problem-artifact-catalog.md)에 정리되어 있습니다.

## 운영 경계

이 저장소는 Kubernetes 실습과 로컬 개발을 위한 프로젝트입니다. 백엔드가 Docker socket에 접근해 privileged k3s 컨테이너를 만들기 때문에, 신뢰할 수 있는 개인 개발 환경에서만 실행해야 합니다. 공개 인터넷에 그대로 배포하거나 다중 테넌트 서비스로 운영하기 위한 보안·격리·관측성 검증은 아직 범위에 포함하지 않았습니다.

기존 `POOL_SIZE` 설정은 generation과 allocation 소유권을 보존하지 못하므로 현재 Runner에서는 `0`만 허용합니다. 0보다 큰 값이면 서버가 시작을 거부합니다.

공개 서비스용 실행 환경은 별도 커널을 제공하는 Proxmox/KVM Runner로 옮기는 중입니다. 구현·격리·장애 복구의 공개 게이트가 통과되기 전까지 `local-docker`를 인터넷이나 홈 LAN에 노출하면 안 됩니다. 설계와 남은 게이트는 [Runner 플랫폼 아키텍처](docs/architecture/runner-platform.md)와 [공개 서비스 전환 로드맵](docs/public-service-roadmap.md)을 참고하세요.

## 더 읽기

- [전체 구현 계획과 API/WebSocket 계약](PLAN.md)
- [문제 artifact catalog와 공개 경계](docs/architecture/adr/002-problem-artifact-catalog.md)
- [개발 기록과 검증 결과](docs/development-history.md)
- [환경 변수 예시](.env.example)
- [CI 설정](.github/workflows/ci.yaml)

## 배포 계획

현재 구현은 신뢰할 수 있는 개발 환경을 위한 단계입니다. 향후 Proxmox/KVM Runner로
세션 실행 환경을 분리하고, TLS·백업·모니터링·복구 검증을 마친 뒤 제한된 베타부터
배포할 계획입니다. 현재 저장소의 공개는 서비스 배포 완료를 의미하지 않습니다.
