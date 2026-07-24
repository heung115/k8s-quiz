# Development History — K8s Quiz

이 문서는 초기 구현부터 실제 Docker E2E 검증까지 발견한 문제와 변경
내역을 기록한 개발 로그다. 현재 동작과 실행 방법은 루트 `README.md`와
코드를 기준으로 한다.

## 완료된 태스크

### 태스크 1: 세션 REST 엔드포인트 추가
- `backend/internal/session/handler.go` 신규 생성
- `GET /api/sessions/current` — 활성 세션 정보 반환 (session_id, problem_id, status, timeout_at)
- `DELETE /api/sessions/current` — 세션 종료, 204 반환
- `main.go`에 라우트 등록 완료

### 태스크 2: Dockerfile Go 버전 수정
- `backend/Dockerfile`: `golang:1.22-alpine` → `golang:1.25-alpine`

### 태스크 3: verify 결과 WebSocket 푸시
- `session.Service`에 `SetVerifyCallback` 추가
- `Verify()` 완료 시 콜백으로 `verify_result` 메시지 전송
- `main.go`에서 hub.SendToUser로 연결

### 태스크 4: Docker 네트워크 격리
- `DockerManager.ensureNetwork()` — 사용자별 `--internal` 네트워크 생성
- `DockerManager.Remove()` — 컨테이너 삭제 시 해당 네트워크도 정리
- `CreateOpts.NetworkMode`에 `k8s-quiz-{userID}` 자동 설정

### 태스크 5: hint.md 로딩
- GitLoader가 hint.md를 읽도록 구현 확인
- 6개 문제 전체에 `hint.md` 존재 (pod-crashloop, network-dns-fail, pv-pending, rbac-denied, node-taint, configmap-typo)

### 태스크 6: 프론트엔드 타이머
- `ProblemPage.tsx`에 `useCountdown` 훅 추가
- `timeout_at` 기반 카운트다운, 5분 이하 시 빨간색(danger) + pulse 애니메이션
- 0초 도달 시 자동 세션 종료 + 대시보드 이동

### 태스크 7: 문제 4개 추가 (총 6개)
- `problems/pv-pending/` — PVC Pending (storage, easy, fix)
- `problems/rbac-denied/` — RBAC 권한 거부 (rbac, medium, fix)
- `problems/node-taint/` — Node Taint (scheduling, easy, fix)
- `problems/configmap-typo/` — ConfigMap 키 오타 (config, easy, find+choice)
- 각 문제: problem.yaml, setup.sh, verify.sh, hint.md 포함

### 태스크 8: 백엔드 단위 테스트
- `internal/auth/service_test.go` — JWT 생성/검증/만료/해시
- `internal/session/service_test.go` — 세션 시작/종료/중복/검증/선택지/콜백/정리
- `internal/problem/loader_test.go` — YAML 파싱, hint.md, 기본값, 스크립트
- `internal/problem/handler_test.go` — httptest로 list/get/start/verify/submit 엔드포인트 (Handler 의존성을 인터페이스로 추상화하여 mock 주입)
- `pkg/middleware/auth_test.go` — Auth/AdminOnly 미들웨어
- `go test ./... -short` 전체 통과, `go vet` 클린

### 태스크 9: 프론트엔드 빌드 검증
- `npm run build` (tsc + vite) 통과
- vite.config.ts 프록시 설정 정상 (/api, /ws → localhost:8080)

### 태스크 10: de-ai-web-design 적용
- `lucide-react` 설치, 인터페이스 이모지(⎈ 🔧 🔍 💡 ↺ ✓ ✗ 등)를 전부 Lucide 아이콘으로 교체
- **시맨틱 디자인 토큰 중앙화**: `tailwind.config.ts`에 canvas/surface/edge/ink/accent/success/warning/danger 팔레트 정의
- **컴포넌트 하드코딩 색상 완전 제거**: 모든 페이지/컴포넌트가 시맨틱 토큰 사용 (gray-/blue-/green-/red- 클래스 0건)
- `index.css`에 reduced-motion 대응 추가
- 난이도/상태 배지에 문자 prefix(E/M/H) + 아이콘 이중 신호 추가
- **대시보드 계층 구조 개선**: 동일 크기 반복 카드 그리드 → 난이도별 그룹 섹션(Easy/Medium/Hard 헤더 + 설명 + 난이도 악센트 바 카드로 재구성)
- xterm 테마도 시맨틱 팔레트와 일치시킴
- 접근성: aria-hidden, aria-label, 포커스/대비 유지

### 태스크 11: README 보강
- 프로젝트 구조 트리, API 엔드포인트 전체 목록, WebSocket 프로토콜, 문제 추가 가이드, 환경 변수 표, 테스트 명령어

## 검증 결과

```
go build ./...          ✓ 통과
go vet ./...            ✓ 클린
go test ./... -short    ✓ 5개 패키지 통과 (auth, problem, session, middleware)
npm run build           ✓ tsc + vite 빌드 성공
하드코딩 색상 검사        ✓ 0건 (전부 시맨틱 토큰)
```

## 추가 작업 — v1 로드맵 마무리

### 터미널 resize handling (PLAN Phase 4)
- `container.Manager.ExecInteractive`가 `TerminalSession`(io.ReadWriteCloser + Resize) 반환하도록 인터페이스 변경
- Docker SDK(`github.com/docker/docker` v27.5.1) 도입, TTY를 할당한 exec 세션 생성
- `ContainerExecResize`로 런타임 터미널 크기 변경, WebSocket `resize` 메시지를 실제 resize로 연결
- go-connections v0.5.0 고정 (v0.7.0의 sockets.DialPipe 제거 호환 문제)

### 컨테이너 크래시 감지 (PLAN Architecture Decisions)
- `session.Service.crashWatcher` goroutine(5초 주기) + 테스트 가능한 `checkCrashes()` 분리
- 활성 세션의 컨테이너가 중지되면 `StatusFailed`로 마킹
- WebSocket으로 `container_crashed` stage + `session_ended(reason=container_crashed)` 푸시
- 프론트엔드: session store에 `crashed` 상태, ProblemPage에 크래시 배너 + 확인 버튼 비활성화, 리셋 유도

### 프론트엔드 세션 복원 (PLAN Phase 7)
- ProblemPage 마운트 시 `GET /api/sessions/current`로 활성 세션 복원 (새로고침 후에도 터미널 재연결)
- Dashboard에 "진행 중인 세션 계속하기" 배너 추가

### 검증 (추가 작업)
- `go build` / `go vet` 클린, `go test ./... -short` 5개 패키지 통과 (container·session 테스트 추가)
- `npm run build` 통과

## Future 항목 진행

### Leaderboard / achievements
- 백엔드: `GET /api/leaderboard`(해결 순위, users JOIN attempts 집계), `GET /api/users/me/achievements`(동적 배지 계산)
- models에 `LeaderboardEntry`, `Achievement` 추가
- 프론트엔드: Leaderboard 페이지(`/leaderboard`) + nav 링크, Profile에 배지 섹션

### Deploy-it problem type
- problem `type`에 `deploy` 추가 (프론트엔드 타입/UI: Rocket 아이콘)
- 예제 문제 `problems/deploy-nginx/` 추가 (총 7개 문제)

### Text grading via LLM
- `pkg/llm` 클라이언트 (OpenAI 호환 `/chat/completions`, PASS/FAIL 파싱)
- config에 `LLM_API_KEY`/`LLM_MODEL`/`LLM_BASE_URL` 추가
- models·migration(002)·repository·loader에 `grading_prompt` 필드
- `session.Service.Verify`가 `verify_type: text`일 때 클러스터 상태 스냅샷을 LLM으로 채점, grader 미설정 시 graceful error

### 검증 (Future)
- `go build` / `go vet` 클린, `go test ./... -short` 6개 패키지 통과 (llm 추가)
- `npm run build` 통과

### Container pre-warm pool
- `pkg/config`: `POOL_SIZE`(기본 0=비활성화)·`POOL_IMAGE`(기본 k3s-base:latest) 추가, `getEnvInt` 헬퍼
- `internal/container/pool.go`: `Pool` 타입 — warm 컨테이너를 ready 채널에 유지(maintain/fill), `Acquire`/`Len`/`Drain`
- `internal/session/service.go`: `SetPool()`, `StartProblem`에서 `pool.Acquire()` 시도, pre-warmed면 부팅 대기 스킵 (`setupEnvironment(..., preWarmed)`)
- `cmd/server/main.go`: `POOL_SIZE>0`일 때 pool 생성·주입, shutdown 시 `Drain`
- 트레이드오프: pool 컨테이너는 default 네트워크 사용 → 네트워크 격리 완화. 그래서 기본 비활성화
- 테스트: `internal/container/pool_test.go` (mock Manager, fill/acquire/drain/failure) — race detector 통과

### 검증 (pre-warm pool)
- `go build` / `go vet` 클린, `go test ./... -short` 전 패키지 통과, pool 테스트 race 클린
- `npm run build` 통과

## Phase 9 마무리 (CI/CD + Testing)

- `.github/workflows/ci.yaml`: `go-version` 1.22 → 1.25 수정 (go.mod `go 1.25.0`과 불일치로 CI 실패하던 버그)
- `internal/session/handler_test.go` 추가: `GET/DELETE /api/sessions/current` httptest 통합 테스트 (세션 없음 404, 세션 복원 200, 종료 204+컨테이너 제거)
- 기존 httptest 커버리지: problem handler, middleware auth, llm client

## 로드맵 전체 수동 감사 (2026-07-23)

PLAN.md 기준 전 항목을 코드와 대조 검증. 아래 4건 버그 발견·수정.

### 수정한 버그
1. **OAuth 콜백 로그인 깨짐 (심각)** — `GithubCallback`이 JSON을 그대로 반환해 브라우저에 raw JSON만 뜨고 SPA가 토큰을 못 받음. 프론트 `Login.tsx`는 `/login?access_token=...&user=...` 리다이렉트 기대. → 리다이렉트로 수정 (`internal/auth/handler.go`), user는 `decodeURIComponent` 계약에 맞춰 이중 인코딩
2. **터미널 연결 시 서버 크래시 (심각)** — `ws/terminal.go`가 `ExecInteractive(nil, ...)`로 nil context 전달 → Docker SDK `http.NewRequestWithContext` panic. → `context.Background()`로 수정
3. **text 채점 UI에서 불가 (중간)** — 백엔드는 `verify_type: text` 지원하나 프론트 verify 버튼이 `script` 전용. → 타입에 `text` 추가, 버튼 조건 확장 (`types/index.ts`, `ProblemPage.tsx`)
4. **timeout_warning 프로토콜 불일치 (경미)** — 전용 타입+`remaining_seconds` 스펙과 달리 `stage` 메시지로 송신, 프론트 처리 코드는 dead code. → `SetTimeoutWarningCallback` 추가해 전용 `MsgTimeoutWarn` 송신 (`session/service.go`, `main.go`)

### 검증 완료 (정상 확인)
- API 라우트 전원 일치 (auth/problems/sessions/users/admin + /ws/terminal). `GET /auth/github`는 리다이렉트라 GET이 정확(스펙의 POST가 부정확)
- DB 스키마 일치 (+002 grading_prompt), WS 메시지 타입 전원 일치
- 크래시 감지(crashWatcher/checkCrashes)·타임아웃 watcher·세션 복원(`GET /api/sessions/current`) 정상
- Leaderboard/achievements 쿼리·핸들러·프론트 계약 일치
- Deploy 문제 타입(deploy-nginx)·k3s-base·Dockerfile(Go 1.25)·docker-compose 일관
- 프론트 WS 처리(Terminal.tsx)·session store 백엔드 콜백과 일치

### 추가 테스트
- `internal/auth/handler_test.go`: 콜백 리다이렉트(invalid_state/missing_code) + 로그인 GitHub 리다이렉트

### 검증
- `go build` / `go vet` / `go test ./... -short` 전 패키지 통과, `npm run build` 통과

## 실환경 Docker E2E 검증 (2026-07-23)

Docker 환경에서 전체 스택 실검증. **플랫폼을 완전히 막고 있던 인프라 버그 3건** 발견·수정.

### 수정한 인프라 버그 (전부 치명적 — 컨테이너가 절대 Ready 안 됐음)
1. **k3s-base 빌드 실패** — `docker/k3s-base/Dockerfile`의 `apk add`가 실패. `rancher/k3s`는 패키지 매니저(apk/apt)가 없는 초경량 이미지. → apk 블록 제거 (이미지에 k3s+kubectl+sh 내장, setup/verify는 curl 등을 Pod 안에서 실행이라 호스트 도구 불필요)
2. **k3s 부팅 실패** — `entrypoint.sh`가 `/usr/local/bin/k3s` 호출하나 실제 경로는 `/bin/k3s`. → 경로 수정
3. **노드 Ready 불가 (cgroup v2)** — Docker Desktop 등 cgroup v2 호스트에서 kubelet이 `kubepods` cgroup 진입 실패. → `internal/container/docker.go` Create에 `--cgroupns=host` 추가

### 검증 결과 (실환경 통과)
- k3s-base 빌드 → privileged 컨테이너 부팅 → `kubectl get nodes` Ready (~9초)
- `docker.go` Create 시퀀스(create→start→exec) 동일 동작 확인
- **문제 라이프사이클 E2E**: pod-crashloop setup.sh(고장: CreateContainerConfigError) → verify.sh FAIL(exit 1) → ConfigMap 수정 → Pod Running/Ready → verify.sh SUCCESS(exit 0)
- `docker compose up --build`: db(healthy)+backend+frontend 기동, 7개 문제 DB 적재, `/health` ok, 인증 미들웨어 401, nginx `/api`·`/ws` 프록시 정상

## 남은 이슈 / 참고사항

- `docker compose up --build` 전체 스택 검증 완료 (Docker Desktop macOS)
- k3s-base 이미지 빌드 검증 완료 (`docker build -t k3s-base:latest docker/k3s-base/`)
- GitHub OAuth App 등록 후 .env에 Client ID/Secret 설정 필요
- 문제의 setup.sh/verify.sh는 실제 k3s 컨테이너에서만 동작
- 프론트엔드 번들 500kB 경고 — 추후 code-splitting 고려 가능 (기능 영향 없음)
- 프론트엔드 테스트 프레임워크 미설치 (Vitest 등 추가 가능)

## 실환경 E2E 재검증 + 인프라 버그 2차 수정 (2026-07-23 오후)

`docker compose up --build` 상태에서 인증된 전체 플로우를 JWT 직접 발급(dev-admin)해 실검증. **컨테이너화된 백엔드에서만 재현되는 치명 버그 2건** 발견·수정.

### 수정한 버그
1. **docker CLI 의존 (치명)** — `internal/container/docker.go`가 Create/Exec/Remove/Logs/IsRunning/RemoveByLabel/네트워크 관리를 `exec.Command("docker", ...)`로 처리하는데, 백엔드 alpine 이미지에는 docker CLI가 없음. 호스트 `go run`에서는 동작하지만 컨테이너 배포에서는 문제 시작이 즉시 실패함. → **Docker SDK 전면 재작성**: ContainerCreate/Start, ContainerExecCreate/Attach/Inspect(stdcopy로 stdout/stderr 분리), ContainerRemove, ContainerLogs, ContainerInspect, ContainerList(label filter), NetworkInspect/Create/Remove. PLAN의 "Initial impl: Docker SDK" 계약과 일치.
2. **`--internal` 네트워크에서 k3s 부팅 실패 (치명)** — 사용자별 네트워크를 `--internal`로 생성하면 기본 게이트웨이가 없어 k3s가 `no default routes found in /proc/net/route` fatal로 사망. → 일반 bridge 네트워크로 변경. 사용자별 네트워크 자체가 사용자 간 격리를 만족(PLAN Security Notes: "no inter-container access"), outbound는 이미지 풀에 필요.
3. **NULL email/avatar_url 스캔 실패 (잠재)** — `user.Repository`가 nullable 컬럼을 일반 string으로 Scan → NULL 행에서 모든 인증 요청 401. → SELECT 3건에 `COALESCE(email,''), COALESCE(avatar_url,'')` 적용.

### E2E 검증 결과 (컨테이너화된 스택, SDK 경로)
- 문제 시작 → k3s-base 컨테이너 생성·부팅 → setup.sh → `status=ready`
- pod-crashloop: `CreateContainerConfigError` 고장 상태 확인 → verify FAIL → ConfigMap 키 추가 → Pod Running/Ready → verify SUCCESS → attempt `success`(duration 기록) → progress 반영
- reset: 컨테이너 재생성 + setup 재실행 → ready
- WebSocket 터미널: auth 메시지 → resize → `kubectl get pods` 입출력 라운드트립 PASS
- choice 문제(configmap-typo): 오답 `success:false`, 정답 `success:true`
- 세션 종료 204 → 컨테이너 0개 + 사용자 네트워크 0개 (완전 정리)
- admin: sync(7), users, attempts 정상

### 추가 테스트 (Phase 9 보강)
- `internal/user/handler_test.go` — Handler의 repo를 `UserRepo` 인터페이스로 추상화(mock 주입), attempts/progress/achievements/leaderboard/admin/role-validation 7개 테스트
- `internal/ws/hub_test.go` — 실제 WebSocket 연결로 register/SendToUser/기존 연결 교체(unregister)/동시 WriteJSON(race clean) 4개 테스트

### 최종 검증
- `go build` / `go vet` 클린, `go test ./... -short -race` 8개 패키지 전체 통과
- `npm run build` 통과
- `docker compose up --build`: health ok, 7문제 적재, 프론트 200, 인증 401/200 정상
- 백엔드 코드에 `exec.Command` 잔존 0건 (CLI 의존 완전 제거)
