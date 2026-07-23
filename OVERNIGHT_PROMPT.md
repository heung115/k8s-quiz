# K8s Quiz — 오버나이트 자율 작업 프롬프트

> 아래 내용을 그대로 Codex에 붙여넣고 자러 가면 됩니다.

---

## 프롬프트 (여기서부터 복사)

```
PLAN.md와 현재 코드를 기반으로 k8s-quiz 프로젝트의 남은 작업을 완료해줘.

## 현재 상태 요약 (내가 분석해둔 것)

백엔드/프론트엔드 스켈레톤은 Phase 1~8까지 대부분 구현되어 있고 `go build ./...`는 통과한다.
하지만 아래 갭들이 있다. 이걸 전부 해결해줘.

## 태스크 목록 (우선순위 순)

### 태스크 1: 버그 수정 — 세션 REST 엔드포인트 누락
- PLAN.md에 `GET /api/sessions/current`, `DELETE /api/sessions/current`가 정의되어 있음
- 프론트엔드 ProblemPage.tsx에서 `api.delete('/api/sessions/current')`를 호출하지만 백엔드에 이 라우트가 없음
- `backend/internal/session/`에 핸들러를 추가하거나 기존 problem handler에 세션 엔드포인트를 등록해
- `GET`은 현재 활성 세션 정보(session_id, problem_id, status, timeout_at)를 반환
- `DELETE`는 EndSession을 호출하고 204 반환

### 태스크 2: 버그 수정 — Backend Dockerfile Go 버전 불일치
- `backend/go.mod`는 `go 1.25.0`인데 `backend/Dockerfile`은 `golang:1.22-alpine` 사용
- Dockerfile의 builder 이미지를 `golang:1.25-alpine`으로 수정

### 태스크 3: 검증 결과를 WebSocket으로도 푸시
- PLAN.md: "Check button → backend runs verify.sh → async → WebSocket pushes result"
- 현재는 REST 응답으로만 결과를 반환하고 WebSocket으로는 안 보냄
- `session.Service.Verify()` 완료 후 `hub.SendToUser()`로 `verify_result` 메시지를 보내도록 수정
- 프론트엔드 Terminal.tsx는 이미 `verify_result` WS 메시지를 처리할 수 있음
- main.go에서 session service가 hub에 접근할 수 있도록 콜백 또는 의존성 주입

### 태스크 4: Docker 네트워크 격리 구현
- PLAN.md: "Network isolation: separate Docker network per user container"
- `DockerManager.Create()`에서 사용자별 Docker 네트워크를 생성하고 컨테이너를 해당 네트워크에 붙여
- 컨테이너 삭제 시 네트워크도 정리
- `CreateOpts`에 이미 `NetworkMode` 필드가 있으니 활용

### 태스크 5: GitLoader가 hint.md 읽도록 수정
- 현재 `problem/loader.go`는 `problem.yaml`만 파싱함
- 각 문제 디렉토리에 `hint.md`가 있으면 읽어서 `Problem.Hint` 필드에 넣어
- `problems/pod-crashloop/hint.md`, `problems/network-dns-fail/hint.md` 파일도 생성
- hint.md 내용은 문제에 대한 단계별 힌트 (스포일러 없이 방향만)

### 태스크 6: 프론트엔드 타이머 표시
- ProblemPage에 남은 시간 카운트다운 타이머 추가
- 세션 시작 시 `timeout_at`을 기준으로 카운트다운
- 5분 이하 남으면 빨간색 경고
- 시간 초과 시 자동 종료 처리 (백엔드에서 이미 timeout watcher 있음)

### 태스크 7: 문제 4개 추가 (총 6개)
- `problems/` 디렉토리에 아래 4개 문제 추가:
  1. `pv-pending/` — PersistentVolumeClaim이 Pending 상태 (storage, easy, fix)
  2. `rbac-denied/` — ServiceAccount가 Pod list 권한 없음 (rbac, medium, fix)
  3. `node-taint/` — Node에 taint가 있어서 Pod가 Pending (scheduling, easy, fix)
  4. `configmap-typo/` — ConfigMap 키 이름 오타로 앱 에러 (config, easy, find+choice)
- 각 문제마다 problem.yaml, setup.sh, verify.sh, hint.md 작성
- setup.sh는 k3s 안에서 실행되며 고장난 상태를 만들어야 함
- verify.sh는 exit 0 = 성공, exit 1 = 실패. false-pass/false-fail 없도록 견고하게
- configmap-typo는 verify_type: choice로 choices와 correct_choice 포함

### 태스크 8: 백엔드 단위 테스트 작성
- `backend/internal/auth/service_test.go` — JWT 생성/검증, 토큰 만료, 리프레시 로직
- `backend/internal/session/service_test.go` — 세션 시작/종료/타임아웃/중복 세션 처리 (container.Manager를 mock)
- `backend/internal/problem/loader_test.go` — GitLoader가 problem.yaml + hint.md 파싱하는지 (temp dir 사용)
- `backend/internal/problem/handler_test.go` — httptest로 API 엔드포인트 테스트 (list, get, start, verify)
- `backend/pkg/middleware/auth_test.go` — Auth 미들웨어, AdminOnly 미들웨어
- mock은 interface 기반 수동 mock (외부 라이브러리 없이)
- `go test ./... -short`가 통과해야 함

### 태스크 9: 프론트엔드 빌드 검증 + 수정
- `cd frontend && npm install && npm run build` 실행
- TypeScript 에러나 빌드 실패가 있으면 수정
- `vite.config.ts`에 dev 서버 프록시 설정 확인 (/api, /ws → localhost:8080)

### 태스크 10: 프론트엔드 디자인 — AI 티 제거 (de-ai-web-design 스킬 적용)
- `de-ai-web-design` 스킬의 워크플로우와 감사 휴리스틱을 따라 프론트엔드 UI를 개선해
- 현재 코드의 AI-default 패턴을 진단하고 수정:
  - 이모지 아이콘(⎈, 🔧, 🔍, 💡, ↺, ✓, ✗ 등)을 Lucide React 아이콘으로 교체
  - 반복되는 카드/배지/상태 필의 시각적 문법을 제품(K8s 트러블슈팅 학습 플랫폼)에 맞게 재정비
  - 색상 체임을 시맨틱 토큰으로 정리 (canvas, surface, text, muted, border, accent, success, warning, danger)
  - Tailwind config에 디자인 토큰을 중앙화하고 컴포넌트에서 하드코딩된 색상 제거
  - 난이도 배지, 카테고리 태그, 상태 표시가 색상만으로 구분되지 않도록 이중 신호 추가
  - 대시보드 문제 카드 그리드의 계층 구조 개선 (동일 크기 반복 카드 탈피)
  - 로그인 페이지, 레이아웃 헤더, 프로필 페이지도 일관된 디자인 언어 적용
- `lucide-react` 패키지 설치 (`npm install lucide-react`)
- 접근성 유지: 키보드 네비게이션, 포커스 표시, 대비, reduced-motion
- 데스크톱/모바일 반응형 유지
- 결과물이 "K8s 트러블슈팅 플랫폼"다운 기술적이고 집중된 느낌이어야 함. 일반적인 AI 대시보드 느낌 X

### 태스크 11: README.md 보강
- 프로젝트 구조 설명 (디렉토리 트리)
- API 엔드포인트 목록 (PLAN.md에서 가져와서 실제 구현과 대조)
- WebSocket 프로토콜 설명
- 문제 추가 가이드 (problem.yaml 스키마, setup.sh/verify.sh 작성법)
- 환경 변수 설명

## 규칙

- 막히면 합리적으로 판단해서 진행해. 나한테 질문하지 마.
- 설계 판단이 필요한 부분은 PLAN.md와 기존 코드 컨벤션을 따라.
- 서브에이전트(create_thread)를 적극 활용해서 병렬로 작업해:
  - 서브에이전트 A: 백엔드 태스크 1~5, 8 (버그 수정 + 기능 + 테스트)
  - 서브에이전트 B: 프론트엔드 태스크 6, 9, 10 (타이머 + 빌드 + 디자인)
  - 서브에이전트 C: 문제 작성 태스크 7 (4개 문제)
  - 메인: 태스크 11 (README) + 서브에이전트 결과 취합 + 충돌 해결
- 각 태스크 완료 후 관련 빌드/테스트로 검증해.
- 모든 태스크가 끝나면 변경 요약을 HANDOFF.md에 작성해.
- 절대 중간에 멈추지 마. 모든 태스크를 완료해.
- git commit은 하지 마.
```

---

## 사용법

1. 위 ``` 안의 내용을 통째로 복사
2. Codex 새 태스크에 붙여넣기
3. Approval policy가 `never`인지 확인 (현재 설정 유지하면 됨)
4. 자러 가기 💤
5. 아침에 HANDOFF.md 확인

## 참고: 현재 코드에서 발견한 갭 요약

| # | 갭 | 심각도 |
|---|-----|--------|
| 1 | `GET/DELETE /api/sessions/current` 라우트 없음 (프론트에서 호출 중) | 🔴 버그 |
| 2 | Dockerfile Go 버전 1.22 vs go.mod 1.25 불일치 | 🔴 빌드 실패 |
| 3 | verify 결과가 WebSocket으로 안 옴 | 🟡 PLAN 미충족 |
| 4 | Docker 네트워크 격리 미구현 | 🟡 보안 |
| 5 | hint.md 로딩 안 됨 | 🟡 기능 누락 |
| 6 | 프론트엔드 타이머 없음 | 🟡 UX |
| 7 | 문제 2개뿐 (PLAN은 6개 카테고리) | 🟡 콘텐츠 |
| 8 | 테스트 0개 | 🟡 품질 |
| 9 | 프론트엔드 빌드 미검증 | 🟡 |
| 10 | AI 티 나는 UI (이모지, 반복 카드, 시맨틱 없는 색상) | 🟡 디자인 |
| 11 | README 부족 | 🟢 문서 |
