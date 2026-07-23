export function Docs() {
  return (
    <div className="max-w-3xl mx-auto px-4 py-8 prose prose-invert prose-sm">
      <h1 className="text-2xl font-bold mb-6">문서</h1>

      <section className="mb-10">
        <h2 className="text-xl font-semibold mb-4 text-accent-hover">사용자 가이드</h2>
        <div className="space-y-4 text-ink">
          <div>
            <h3 className="font-medium text-ink mb-1">1. 문제 선택</h3>
            <p>대시보드에서 카테고리(Pod, Network, Storage, RBAC, Scheduling, Config)와 난이도(easy, medium, hard)로 필터링하여 문제를 선택합니다.</p>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">2. 문제 풀이</h3>
            <p>"문제 시작" 버튼을 클릭하면 k3s 환경이 컨테이너로 생성됩니다. 환경 준비가 완료되면 웹 터미널에서 <code className="bg-surface-2 px-1 rounded">kubectl</code> 등의 명령어로 문제를 해결합니다.</p>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">3. 검증</h3>
            <p>
              <strong>Fix it 문제:</strong> "확인" 버튼을 눌러 검증 스크립트로 자동 판정합니다.<br/>
              <strong>Find it 문제:</strong> 원인을 선택지에서 골라 제출합니다.
            </p>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">4. 리셋 & 종료</h3>
            <p>환경이 망가졌으면 "리셋" 버튼으로 초기화할 수 있습니다. 타임아웃은 유지됩니다. "종료" 버튼으로 세션을 끝냅니다.</p>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">5. 타임아웃</h3>
            <p>문제마다 제한 시간이 있으며(기본 30분), 5분 전부터 경고 메시지가 표시됩니다.</p>
          </div>
        </div>
      </section>

      <section>
        <h2 className="text-xl font-semibold mb-4 text-success">문제 출제 가이드</h2>
        <div className="space-y-4 text-ink">
          <div>
            <h3 className="font-medium text-ink mb-1">문제 구조</h3>
            <p>문제 리포지토리에 디렉토리를 생성하고 아래 파일들을 추가합니다:</p>
            <pre className="bg-surface border border-edge rounded-lg p-4 text-xs overflow-x-auto">
{`problems/
  my-problem/
    problem.yaml    # 필수: 메타데이터
    setup.sh        # 필수: 고장난 환경 구성
    verify.sh       # 필수 (script type): 검증 스크립트
    hint.md         # 선택: 힌트`}
            </pre>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">problem.yaml</h3>
            <pre className="bg-surface border border-edge rounded-lg p-4 text-xs overflow-x-auto">
{`id: my-problem
title: "문제 제목"
description: |
  문제 설명을 작성합니다.
category: pod          # pod|network|storage|rbac|scheduling|config
difficulty: easy       # easy|medium|hard
type: fix              # fix|find
timeout_minutes: 30
verify_type: script    # script|choice
base_image: k3s-base:latest
# choices:             # choice type일 때
#   - id: a
#     text: "선택지 A"
# correct_choice: a`}
            </pre>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">setup.sh</h3>
            <p>k3s가 부팅된 후 실행됩니다. 고장난 상태를 만듭니다.</p>
            <pre className="bg-surface border border-edge rounded-lg p-4 text-xs overflow-x-auto">
{`#!/bin/sh
# 예: Pod을 CrashLoopBackOff 상태로 만들기
kubectl create deployment broken-app --image=nginx
kubectl set env deployment/broken-app INVALID_ENV=crash`}
            </pre>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">verify.sh</h3>
            <p>exit code 0이면 성공, 그 외는 실패입니다.</p>
            <pre className="bg-surface border border-edge rounded-lg p-4 text-xs overflow-x-auto">
{`#!/bin/sh
# 예: Pod이 Running인지 확인
STATUS=$(kubectl get pods -l app=broken-app -o jsonpath='{.items[0].status.phase}')
if [ "$STATUS" = "Running" ]; then
  echo "Pod is running!"
  exit 0
else
  echo "Pod is not running: $STATUS"
  exit 1
fi`}
            </pre>
          </div>
          <div>
            <h3 className="font-medium text-ink mb-1">등록</h3>
            <p>문제 리포에 push 후, Admin 페이지에서 "Sync Problems" 버튼을 클릭하면 DB에 반영됩니다.</p>
          </div>
        </div>
      </section>
    </div>
  )
}
