import { Link } from 'react-router-dom'
import { useAuthStore } from '../stores/auth'
import { ThemeToggle } from '../components/ThemeToggle'
import { SectionLabel } from '../components/ui'

function Code({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-[12.5px] bg-surface-2 border border-edge-soft px-1.5 py-0.5 text-info">{children}</code>
}

function Block({ children }: { children: React.ReactNode }) {
  return (
    <pre className="font-mono text-xs bg-terminal border border-edge p-4 overflow-x-auto leading-relaxed text-ink-muted">{children}</pre>
  )
}

function Item({ n, title, children }: { n: string; title: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-4">
      <span className="font-display font-bold text-edge text-2xl select-none shrink-0 w-10">{n}</span>
      <div className="pb-8 border-l border-edge-soft pl-5 -ml-5">
        <h3 className="font-display font-semibold text-lg">{title}</h3>
        <div className="mt-2 text-sm text-ink-muted leading-relaxed space-y-2">{children}</div>
      </div>
    </div>
  )
}

export function Docs() {
  const { user } = useAuthStore()
  return (
    <>
    <header className="border-b border-edge bg-canvas/90 backdrop-blur sticky top-0 z-40">
      <div className="max-w-3xl mx-auto px-5 h-14 flex items-center justify-between">
        <Link to="/" className="flex items-center gap-2 font-display font-bold">
          <span className="w-7 h-7 bg-accent text-white flex items-center justify-center text-base" aria-hidden="true">⎈</span>
          K8S<span className="text-accent-hover">QUIZ</span>
        </Link>
        <div className="flex items-center gap-3">
          <ThemeToggle />
          {user
            ? <Link to="/" className="btn-ghost text-sm py-1.5">콘솔</Link>
            : <Link to="/login" className="btn-primary text-sm py-1.5">시작하기</Link>}
        </div>
      </div>
    </header>
    <div className="max-w-3xl mx-auto px-5 py-10">
      <SectionLabel>DOCUMENTATION</SectionLabel>
      <h1 className="mt-3 font-display font-bold text-3xl tracking-tight">문서</h1>
      <p className="mt-2 text-sm text-ink-muted">처음이라면 사용자 가이드부터, 문제를 내고 싶다면 제작 가이드를 보세요.</p>

      {/* ---- user guide ---- */}
      <section className="mt-12">
        <SectionLabel className="mb-6">USER GUIDE</SectionLabel>
        <Item n="01" title="시나리오 선택">
          <p>콘솔에서 카테고리(Pod · Network · Storage · RBAC · Scheduling · Config)와 난이도(SEV-3=easy, SEV-2=medium, SEV-1=hard)로 좁혀서 시나리오를 고릅니다. 검색도 됩니다.</p>
        </Item>
        <Item n="02" title="환경 시작">
          <p><strong className="text-ink">환경 시작</strong>을 누르면 격리된 k3s 클러스터가 컨테이너로 뜹니다. 부팅 → 시나리오 주입(setup.sh) → 대기 단계를 타임라인으로 보여줍니다. 준비되면 웹 터미널에서 <Code>kubectl</Code>을 쓸 수 있습니다.</p>
        </Item>
        <Item n="03" title="진단과 수정">
          <p>터미널은 실제 컨테이너의 셸입니다. <Code>kubectl get pods</Code>, <Code>kubectl describe</Code>, <Code>kubectl logs</Code> 등으로 증상을 추적하고 원인을 직접 고칩니다.</p>
        </Item>
        <Item n="04" title="검증">
          <p><strong className="text-ink">FIX 시나리오</strong>는 <strong className="text-ink">검증</strong> 버튼이 verify.sh를 실행해 판정합니다. 로그까지 함께 표시됩니다.<br />
          <strong className="text-ink">FIND 시나리오</strong>는 원인을 선택지에서 골라 제출합니다.</p>
        </Item>
        <Item n="05" title="리셋 · 종료 · 타임아웃">
          <p>환경이 더 망가졌으면 <strong className="text-ink">리셋</strong>으로 초기 상태부터 다시 시작할 수 있습니다(제한 시간은 유지). <strong className="text-ink">종료</strong>는 세션을 끝내고 컨테이너를 즉시 정리합니다. 제한 시간 5분 전부터 경고가 표시됩니다.</p>
        </Item>
      </section>

      {/* ---- authoring guide ---- */}
      <section className="mt-8">
        <SectionLabel className="mb-6">PROBLEM AUTHORING GUIDE</SectionLabel>
        <div className="text-sm text-ink-muted leading-relaxed space-y-4">
          <p>문제는 <Code>problems/&lt;id&gt;/</Code> 디렉터리 아래 4개 파일로 구성됩니다.</p>
          <Block>{`problems/my-problem/
  problem.yaml   # 메타데이터
  setup.sh       # 고장난 상태를 만드는 스크립트
  verify.sh      # 해결 여부를 판정 (exit 0 = 성공)
  hint.md        # 선택 힌트`}</Block>
          <div>
            <p className="font-display font-semibold text-ink mb-2">problem.yaml 예시</p>
            <Block>{`id: my-problem
title: "문제 제목"
description: |
  상황 설명…
category: pod            # pod|network|storage|rbac|scheduling|config
difficulty: easy         # easy|medium|hard
type: fix                # fix|find|deploy
timeout_minutes: 30
verify_type: script      # script|choice
base_image: k3s-base:latest`}</Block>
          </div>
          <div>
            <p className="font-display font-semibold text-ink mb-2">검증 스크립트 원칙</p>
            <ul className="list-none space-y-1.5">
              <li><span className="text-success font-mono">✓</span> exit 0 = 해결, exit 1 = 미해결. 메시지는 사용자에게 그대로 표시됩니다.</li>
              <li><span className="text-success font-mono">✓</span> 상태가 실제로 원하는 조건을 만족하는지 확인하세요 (예: Pod가 Running이면서 Ready).</li>
              <li><span className="text-danger font-mono">✗</span> "파일이 존재한다"처럼 우회 가능한 판정은 금물 — 거짓 통과(false-pass)가 납니다.</li>
            </ul>
          </div>
          <p>작성 후 관리자 페이지의 <strong className="text-ink">동기화</strong>를 누르면 Git 저장소에서 다시 읽어 반영됩니다.</p>
        </div>
      </section>
    </div>
    </>
  )
}
