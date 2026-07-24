# K8s Quiz

웹 기반 Kubernetes 트러블슈팅 플랫폼. 고장난 k3s 환경을 웹 터미널에서 직접 해결하고 검증받습니다.

## 프로젝트 상태

- 개인 프로젝트로 개발 중인 로컬 프로토타입입니다.
- 사용자별 k3s 컨테이너를 실행하므로 신뢰할 수 있는 개발 환경에서만
  사용해야 합니다.
- 공개 서비스 트래픽이나 프로덕션 용량을 검증한 프로젝트가 아닙니다.
- 보안 점검 결과와 남은 위험은 [`SECURITY.md`](SECURITY.md)에
  기록합니다.

## AI-assisted development

Codex, Cursor, ChatGPT, Antigravity를 개발 보조 도구로 사용했습니다.
저장소 안의 agent harness는 backend, frontend, container, problem
authoring, QA 역할과 handoff 경계를 정의합니다.

- Team spec: [`docs/harness/k8s-quiz/team-spec.md`](docs/harness/k8s-quiz/team-spec.md)
- Specialist skills: [`.agents/skills/`](.agents/skills/)
- Coordination artifacts: [`_workspace/`](_workspace/)
- Development log: [`docs/development-history.md`](docs/development-history.md)

AI가 제안한 변경은 코드 반영만으로 완료 처리하지 않고 Go test,
TypeScript build, Docker Compose E2E, 실제 k3s 문제의 fail→fix→pass,
보안 감사와 재현 가능한 부하·query-plan 실험으로 확인합니다.

## Quick Start

```bash
# 1. 환경 변수 설정
cp .env.example .env
# .env 파일에서 GitHub OAuth Client ID/Secret 설정

# 2. k3s-base 이미지 빌드
docker build -t k3s-base:latest docker/k3s-base/

# 3. 전체 스택 실행
docker compose up --build

# 4. 접속
# Frontend: http://localhost:5173
# Backend API: http://localhost:8080
```

## 로컬 개발

```bash
# Backend
cd backend
go run ./cmd/server

# Frontend
cd frontend
npm install
npm run dev

# DB (Docker)
docker run -d --name k8s-quiz-db \
  -e POSTGRES_USER=k8squiz \
  -e POSTGRES_PASSWORD=k8squiz \
  -e POSTGRES_DB=k8squiz \
  -p 5432:5432 postgres:16-alpine

# Migration
migrate -path backend/migrations -database "postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable" up
```

## 로컬 부하 테스트

실행 중인 백엔드의 `/health` 엔드포인트를 기본 200 RPS로 30초간
검증합니다. 이 테스트는 로컬 기준선 확인용이며 실제 사용자 트래픽이나
프로덕션 용량을 의미하지 않습니다.

```bash
k6 run load-tests/health.js
```

조건은 환경변수로 변경할 수 있습니다.

```bash
BASE_URL=http://127.0.0.1:8080 \
TARGET_RPS=300 \
DURATION=60s \
MAX_VUS=150 \
k6 run load-tests/health.js
```

### PostgreSQL query plan

실제 attempt 조회 query를 10만 건 합성 데이터로 재현해
`EXPLAIN ANALYZE` 결과를 비교합니다. 전체 작업은 transaction에서
실행한 뒤 rollback하므로 합성 데이터가 남지 않습니다.

```bash
docker compose exec -T db \
  psql -U k8squiz -d k8squiz -f - \
  < load-tests/postgres_attempts_index.sql
```

측정 결과와 적용·기각한 index는
[`docs/postgres-query-plan-2026-07-24.md`](docs/postgres-query-plan-2026-07-24.md)에
기록했습니다.

## 프로젝트 구조

```
k8s-quiz/
├── backend/
│   ├── cmd/server/main.go          # 서버 엔트리포인트
│   ├── internal/
│   │   ├── auth/                   # GitHub OAuth + JWT (handler, service)
│   │   ├── container/              # ContainerManager 인터페이스 + Docker 구현
│   │   ├── problem/                # 문제 CRUD, GitLoader, Repository
│   │   ├── session/                # 세션 라이프사이클, 타임아웃, 검증
│   │   ├── user/                   # 사용자 관리, 진행률
│   │   └── ws/                     # WebSocket Hub + 터미널 핸들러
│   ├── migrations/                 # PostgreSQL 마이그레이션
│   ├── pkg/
│   │   ├── config/                 # 환경 변수 설정
│   │   ├── middleware/             # Auth, AdminOnly 미들웨어
│   │   └── models/                 # 공유 데이터 모델
│   └── Dockerfile
├── frontend/
│   ├── src/
│   │   ├── api/client.ts           # API 클라이언트 (토큰 리프레시 포함)
│   │   ├── components/             # Layout, Terminal (xterm.js)
│   │   ├── pages/                  # Dashboard, ProblemPage, Login, Profile, Admin, Docs
│   │   ├── stores/                 # Zustand (auth, session)
│   │   └── types/                  # TypeScript 타입 정의
│   ├── nginx.conf                  # 프로덕션 리버스 프록시
│   └── Dockerfile
├── problems/                       # 문제 정의 (Git 기반)
│   ├── pod-crashloop/
│   ├── network-dns-fail/
│   ├── pv-pending/
│   ├── rbac-denied/
│   ├── node-taint/
│   └── configmap-typo/
├── docker/
│   └── k3s-base/                   # k3s 베이스 이미지
├── load-tests/                     # k6 및 PostgreSQL query-plan 실험
├── docs/
│   ├── harness/                    # repo-local agent team spec
│   ├── development-history.md      # 구현·E2E 검증 기록
│   └── *-2026-07-24.md             # 재현 가능한 성능 기준선
├── .agents/skills/                 # 역할별 개발·QA harness
├── docker-compose.yaml
├── .github/workflows/ci.yaml
└── .env.example
```

## API 엔드포인트

### Auth (공개)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/auth/github` | GitHub OAuth 시작 |
| GET | `/api/auth/github/callback` | OAuth 콜백 → 토큰 발급 |
| POST | `/api/auth/refresh` | Access Token 갱신 |
| GET | `/api/auth/me` | 현재 사용자 정보 |

### Problems (인증 필요)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/problems` | 문제 목록 (category, difficulty, type 필터) |
| GET | `/api/problems/:id` | 문제 상세 |
| POST | `/api/problems/:id/start` | 문제 시작 → 컨테이너 생성 |
| POST | `/api/problems/:id/reset` | 환경 리셋 |
| POST | `/api/problems/:id/verify` | 검증 실행 (async) |
| POST | `/api/problems/:id/submit` | 선택지 제출 (find+choice) |

### Sessions (인증 필요)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/sessions/current` | 현재 활성 세션 |
| DELETE | `/api/sessions/current` | 세션 종료 |

### Users (인증 필요)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/users/me/attempts` | 내 시도 기록 |
| GET | `/api/users/me/progress` | 내 진행률 |
| GET | `/api/users/me/achievements` | 내 배지 |

### Leaderboard (인증 필요)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/leaderboard` | 해결 순위 (상위 50명) |

### Admin (관리자)
| Method | Path | 설명 |
|--------|------|------|
| GET | `/api/admin/problems` | 전체 문제 목록 |
| POST | `/api/admin/problems` | 문제 생성 |
| PUT | `/api/admin/problems/:id` | 문제 수정 |
| DELETE | `/api/admin/problems/:id` | 문제 삭제 |
| POST | `/api/admin/problems/sync` | Git에서 문제 동기화 |
| GET | `/api/admin/users` | 사용자 목록 |
| PUT | `/api/admin/users/:id/role` | 역할 변경 |
| GET | `/api/admin/attempts` | 전체 시도 기록 |

## WebSocket 프로토콜

연결: `ws://host/ws/terminal`

### 인증
연결 후 첫 번째 메시지로 JWT 전송:
```json
{"type": "auth", "token": "<access_token>"}
```

### 메시지 타입
| Type | Direction | 설명 |
|------|-----------|------|
| `auth` | Client → Server | JWT 인증 |
| `input` | Client → Server | 터미널 입력 |
| `resize` | Client → Server | 터미널 크기 변경 (cols, rows) |
| `output` | Server → Client | 터미널 출력 |
| `stage` | Server → Client | 단계 진행 (stage, message) |
| `verify_result` | Server → Client | 검증 결과 (success, log) |
| `timeout_warning` | Server → Client | 타임아웃 경고 (remaining_seconds) |
| `session_ended` | Server → Client | 세션 종료 (reason) |
| `error` | Server → Client | 오류 (message) |

## 문제 추가 가이드

`problems/` 디렉토리에 새 폴더를 만들고 아래 파일들을 작성합니다.

### problem.yaml
```yaml
id: my-problem              # 고유 ID (폴더명과 동일)
title: "문제 제목"
description: |
  문제 설명 (마크다운 지원)
category: pod               # pod | network | storage | rbac | scheduling | config
difficulty: easy            # easy | medium | hard
type: fix                   # fix | find | deploy
timeout_minutes: 30
verify_type: script         # script | choice | text
base_image: k3s-base:latest
# grading_prompt: |         # verify_type: text일 때 LLM 채점 기준
#   모든 Pod가 Running 상태인지 확인하세요.
# choices:                  # verify_type: choice인 경우
#   - id: a
#     text: "선택지 A"
# correct_choice: a
```

### setup.sh
k3s 부팅 후 컨테이너 내부에서 실행됩니다. 고장난 상태를 만듭니다.
```bash
#!/bin/sh
kubectl apply -f - <<'YAML'
# 고장난 리소스 정의
YAML
sleep 5
```

### verify.sh
사용자가 문제를 해결했는지 검증합니다. exit 0 = 성공, exit 1 = 실패.
```bash
#!/bin/sh
STATUS=$(kubectl get pods -l app=my-app -o jsonpath='{.items[0].status.phase}')
if [ "$STATUS" = "Running" ]; then
  echo "SUCCESS"
  exit 0
fi
echo "FAIL: status=$STATUS"
exit 1
```

### hint.md (선택)
사용자에게 표시될 힌트. 스포일러 없이 방향만 제시합니다.

작성 후 Admin 페이지에서 "Sync Problems"를 클릭하거나 서버를 재시작하면 DB에 반영됩니다.

## 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `GITHUB_CLIENT_ID` | (필수) | GitHub OAuth App Client ID |
| `GITHUB_CLIENT_SECRET` | (필수) | GitHub OAuth App Client Secret |
| `JWT_SECRET` | `dev-secret-change-me` | JWT Access Token 서명 키 |
| `JWT_REFRESH_SECRET` | `dev-refresh-secret-change-me` | Refresh Token용 (예비) |
| `DATABASE_URL` | `postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable` | PostgreSQL 연결 |
| `PROBLEMS_REPO_PATH` | `./problems` | 문제 디렉토리 경로 |
| `FRONTEND_URL` | `http://localhost:5173` | CORS 허용 프론트엔드 URL |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker 소켓 경로 |
| `SERVER_PORT` | `8080` | 백엔드 서버 포트 |
| `LLM_API_KEY` | (비어있음) | LLM 채점용 API 키 (text grading, 선택) |
| `LLM_MODEL` | `gpt-4o-mini` | LLM 모델 |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | OpenAI 호환 API 엔드포인트 |
| `POOL_SIZE` | `0` | 컨테이너 pre-warm 풀 크기 (0=비활성화). 활성화 시 시작 지연은 줄지만 warm 컨테이너가 default 네트워크를 써서 세션 네트워크 격리가 완화됨 |
| `POOL_IMAGE` | `k3s-base:latest` | pre-warm 풀에 사용할 이미지 |

## 테스트

```bash
# 백엔드 전체 테스트
cd backend && go test ./... -short

# 프론트엔드 빌드 검증
cd frontend && npm run build
```

## Tech Stack

- **Backend**: Go + Gin, pgx (PostgreSQL), gorilla/websocket
- **Frontend**: React + Vite + TypeScript, Tailwind CSS, Zustand, xterm.js, Lucide Icons
- **Infra**: Docker Compose, k3s-in-Docker, PostgreSQL 16
- **Auth**: GitHub OAuth → JWT (Access 15m + Refresh 7d)
- **CI**: GitHub Actions (build + test)
