# K8s Quiz

[![CI](https://github.com/heung115/k8s-quiz/actions/workflows/ci.yaml/badge.svg)](https://github.com/heung115/k8s-quiz/actions/workflows/ci.yaml)

Kubernetes 문제는 문서로 읽을 때보다, 실제로 한 번 망가뜨리고 고쳐볼 때 더 오래 남습니다. K8s Quiz는 브라우저에서 고장 난 k3s 환경에 접속해 `kubectl`로 원인을 찾고, 해결 결과를 검증받는 실습 플랫폼입니다.

문제를 고르면 사용자 전용 컨테이너가 만들어지고, 준비가 끝나면 웹 터미널이 열립니다. 문제를 해결한 뒤 **확인하기**를 누르면 검증 스크립트가 클러스터 상태를 판정합니다. 종료하거나 시간이 만료되면 컨테이너와 네트워크를 정리합니다.

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

백엔드는 Docker SDK를 통해 실습 컨테이너를 관리합니다. REST API는 문제 목록과 세션 같은 상태를, WebSocket은 터미널 입출력·준비 단계·검증 결과를 담당합니다. 세션은 사용자당 하나만 유지하며, 컨테이너 생성 수 상한과 선택적 pre-warm pool도 설정할 수 있습니다.

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

# 2. 문제 환경에 사용할 k3s 이미지를 빌드합니다.
make image

# 3. 앱과 PostgreSQL을 실행합니다.
docker compose up --build
```

브라우저에서 [http://localhost:5173](http://localhost:5173)으로 접속합니다. 로그인까지 확인하려면 GitHub OAuth App의 callback URL을 아래처럼 등록해야 합니다.

```text
http://localhost:5173/api/auth/github/callback
```

`docker compose` 환경에서는 프론트엔드가 `/api`와 `/ws` 요청을 백엔드로 프록시합니다. 백엔드를 따로 실행할 때는 `.env`의 `DATABASE_URL`과 `PROBLEMS_REPO_PATH`를 현재 환경에 맞게 조정하세요.

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

공개 예제 문제는 [`k8s-quiz-problems`](https://github.com/heung115/k8s-quiz-problems) 저장소에서 관리하며, 이 저장소에는 `problems/` submodule로 연결됩니다. 문제 ID와 같은 이름의 디렉터리를 만들면 됩니다.

```text
problems/
  my-problem/
    problem.yaml
    setup.sh
    verify.sh
    hint.md
```

```yaml
# problem.yaml
id: my-problem
title: "문제 제목"
description: |
  학습자가 확인할 상황과 목표를 적습니다.
category: pod           # pod | network | storage | rbac | scheduling | config
difficulty: easy        # easy | medium | hard
type: fix               # fix | find | deploy
timeout_minutes: 30
verify_type: script     # script | choice | text
base_image: k3s-base:latest
```

`setup.sh`는 고장 난 상태를 만들고, `verify.sh`는 해결 여부를 확인합니다. 검증 스크립트는 exit code 0을 성공으로, 1을 실패로 처리합니다. Kubernetes가 수렴하는 시간을 고려해 유한 폴링으로 작성하고, 조건을 확인하는 명령이 실패를 가리지 않도록 fail-closed 방식으로 작성하는 것을 권장합니다.

문제 저장소에서 변경을 병합한 뒤 메인 저장소에서 `make problems-update`로 submodule 포인터를 갱신합니다. 이어서 Admin 화면에서 **Sync Problems**를 실행하거나 서버를 다시 시작하면 로더가 유효한 문제만 데이터베이스에 반영합니다. ID, 카테고리, 난이도, 검증 방식의 검증 규칙은 [구현 계획](PLAN.md)에 정리했습니다.

## 운영 경계

이 저장소는 Kubernetes 실습과 로컬 개발을 위한 프로젝트입니다. 백엔드가 Docker socket에 접근해 privileged k3s 컨테이너를 만들기 때문에, 신뢰할 수 있는 개인 개발 환경에서만 실행해야 합니다. 공개 인터넷에 그대로 배포하거나 다중 테넌트 서비스로 운영하기 위한 보안·격리·관측성 검증은 아직 범위에 포함하지 않았습니다.

`POOL_SIZE`를 켜면 시작 시간은 줄일 수 있지만 warm 컨테이너가 기본 Docker 네트워크를 사용합니다. 그래서 기본값은 `0`이며, 네트워크 격리가 더 중요한 환경에서는 활성화하지 않는 편이 좋습니다.

## 더 읽기

- [전체 구현 계획과 API/WebSocket 계약](PLAN.md)
- [개발 과정과 Docker E2E 검증 기록](docs/development-history.md)
- [환경 변수 예시](.env.example)
- [CI 설정](.github/workflows/ci.yaml)

## 공개 전 참고

소스 공개 자체는 환영하지만, 다른 사람이 재사용할 수 있게 하려면 공개 전에 원하는 라이선스를 선택해 `LICENSE` 파일을 추가하는 편이 좋습니다. 현재는 라이선스가 포함되어 있지 않습니다.
