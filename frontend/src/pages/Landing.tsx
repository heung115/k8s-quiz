import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { TerminalReplay, SectionLabel, SevTag, TypeTag, SolveStamp, categoryMeta } from '../components/ui'
import { useAuthStore } from '../stores/auth'
import { ThemeToggle } from '../components/ThemeToggle'
import {
  ArrowRight,
  BookOpen, Trophy, ChevronRight,
} from 'lucide-react'

/* scroll-reveal: adds .is-in when the element enters the viewport */
function useReveal<T extends HTMLElement>() {
  const ref = useRef<T | null>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const io = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          el.classList.add('is-in')
          io.disconnect()
        }
      },
      { threshold: 0.12 }
    )
    io.observe(el)
    return () => io.disconnect()
  }, [])
  return ref
}

function Reveal({ children, className = '', delay = 0 }: { children: React.ReactNode; className?: string; delay?: number }) {
  const ref = useReveal<HTMLDivElement>()
  return (
    <div ref={ref} className={`reveal ${className}`} style={{ transitionDelay: `${delay}ms` }}>
      {children}
    </div>
  )
}

const SYMPTOMS = [
  'CrashLoopBackOff', 'ImagePullBackOff', 'CreateContainerConfigError', 'Pending',
  'OOMKilled', 'Evicted', 'NodeNotReady', 'DNS lookup failed', 'PermissionDenied',
  'FailedScheduling', 'BackOff', 'ErrImagePull',
]


const TEASERS = [
  { id: 'pod-crashloop',    code: 'INC-101', title: 'CrashLoopBackOff 해결',        category: 'pod',        difficulty: 'easy',   type: 'fix',    desc: '배포 직후 Pod이 무한 재시작됩니다. 이벤트를 읽어 원인을 제거하세요.', timeout: 30 },
  { id: 'network-dns-fail', code: 'INC-102', title: 'DNS 해석 실패 진단',           category: 'network',    difficulty: 'medium', type: 'fix',    desc: 'Pod 안에서 외부 도메인이 해석되지 않습니다. CoreDNS부터 뜯어보세요.', timeout: 30 },
  { id: 'rbac-denied',      code: 'INC-103', title: 'RBAC 권한 거부 해결',          category: 'rbac',       difficulty: 'medium', type: 'fix',    desc: 'ServiceAccount가 Pod 목록 조회를 거부당합니다. Role 바인딩이 비어 있습니다.', timeout: 30 },
  { id: 'pv-pending',       code: 'INC-104', title: 'PVC Pending 해결',             category: 'storage',    difficulty: 'easy',   type: 'fix',    desc: 'Volume claim이 끝까지 Pending입니다. 스토리지 클래스가 수상합니다.', timeout: 30 },
]

const SUBSYSTEMS = [
  { key: 'pod',        count: 1 },
  { key: 'network',    count: 1 },
  { key: 'storage',    count: 1 },
  { key: 'rbac',       count: 1 },
  { key: 'scheduling', count: 2 },
  { key: 'config',     count: 1 },
]

function GithubMark({ className = 'w-5 h-5' }: { className?: string }) {
  return (
    <svg className={className} fill="currentColor" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.122 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>
    </svg>
  )
}

export function Landing() {
  const { user } = useAuthStore()
  const [scrolled, setScrolled] = useState(false)

  useEffect(() => {
    const onScroll = () => setScrolled(window.scrollY > 24)
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [])

  return (
    <div className="min-h-screen bg-canvas bg-grid">

      {/* ---------- nav ---------- */}
      <header className={`fixed top-0 inset-x-0 z-50 transition-all duration-200 ${scrolled ? 'bg-canvas/90 backdrop-blur border-b border-edge-soft' : 'bg-transparent'}`}>
        <div className="max-w-6xl mx-auto px-5 h-16 flex items-center justify-between">
          <a href="#top" className="flex items-center gap-2.5 font-display font-bold text-lg tracking-wide">
            <span className="w-8 h-8 bg-accent text-white flex items-center justify-center text-xl shadow-[0_0_20px_rgba(50,108,229,0.5)]" aria-hidden="true">⎈</span>
            K8S<span className="text-accent-hover">QUIZ</span>
          </a>
          <nav className="hidden md:flex items-center gap-7 text-sm">
            <a href="#how" className="text-ink-muted hover:text-ink transition-colors">작동 방식</a>
            <a href="#scenarios" className="text-ink-muted hover:text-ink transition-colors">시나리오</a>
            <Link to={user ? '/leaderboard' : '/login'} className="text-ink-muted hover:text-ink transition-colors inline-flex items-center gap-1.5">
              <Trophy className="w-3.5 h-3.5" aria-hidden="true" /> 리더보드
            </Link>
            <Link to="/docs" className="text-ink-muted hover:text-ink transition-colors inline-flex items-center gap-1.5">
              <BookOpen className="w-3.5 h-3.5" aria-hidden="true" /> 문서
            </Link>
          </nav>
          <div className="flex items-center gap-3">
            <ThemeToggle className="hidden sm:inline-flex" />
            {user ? (
              <Link to="/" className="btn-primary text-sm py-2">콘솔로 이동 <ArrowRight className="w-4 h-4" aria-hidden="true" /></Link>
            ) : (
              <a href="/api/auth/github" className="btn-primary text-sm py-2">
                <GithubMark className="w-4 h-4" /> GitHub로 시작
              </a>
            )}
          </div>
        </div>
      </header>

      {/* ---------- hero ---------- */}
      <section id="top" className="max-w-6xl mx-auto px-5 pt-36 pb-20">
        <div className="grid lg:grid-cols-[1.05fr_1fr] gap-14 items-center">
          <div>
            <Reveal>
              <SectionLabel>KUBERNETES TROUBLESHOOTING RANGE</SectionLabel>
            </Reveal>
            <Reveal delay={80}>
              <h1 className="mt-5 font-display font-bold text-[44px] leading-[1.08] sm:text-6xl tracking-tight">
                클러스터는 깨졌다.
                <br />
                <span className="text-accent-hover">고치는 건 당신이다.</span>
              </h1>
            </Reveal>
            <Reveal delay={160}>
              <p className="mt-6 text-ink-muted text-lg leading-relaxed max-w-xl">
                버튼 한 번이면 실제로 고장 난 k3s 클러스터가 뜹니다.
                웹 터미널에서 <span className="font-mono text-[15px] text-info">kubectl</span>로 원인을 진단하고,
                고치고, 검증 스크립트로 통과를 확인하세요.
              </p>
            </Reveal>
            <Reveal delay={240}>
              <div className="mt-8 flex flex-wrap items-center gap-4">
                {user ? (
                  <Link to="/" className="btn-primary">문제 풀러 가기 <ArrowRight className="w-4 h-4" aria-hidden="true" /></Link>
                ) : (
                  <a href="/api/auth/github" className="btn-primary">
                    <GithubMark className="w-4 h-4" /> GitHub로 시작하기
                  </a>
                )}
                <a href="#scenarios" className="btn-ghost">시나리오 둘러보기</a>
              </div>
            </Reveal>
            <Reveal delay={320}>
              <dl className="mt-12 flex flex-wrap gap-y-5 border-t border-edge-soft pt-5">
                {[
                  ['7', '시나리오'],
                  ['6', '서브시스템'],
                  ['~15분', '평균 소요'],
                  ['100%', '실습 기반'],
                ].map(([v, k], i) => (
                  <div key={k} className={`pr-6 ${i > 0 ? 'pl-6 border-l border-edge-soft' : ''}`}>
                    <dt className="micro text-ink-faint">{k}</dt>
                    <dd className="font-display font-bold text-2xl mt-1">{v}</dd>
                  </div>
                ))}
              </dl>
            </Reveal>
          </div>
          <Reveal delay={200} className="relative">
            <div className="absolute -inset-3 border border-accent/20 translate-x-3 translate-y-3" aria-hidden="true" />
            <TerminalReplay />
            <p className="mt-3 micro text-ink-faint text-right">pod-crashloop 시나리오 실제 세션 재연</p>
          </Reveal>
        </div>
      </section>

      {/* ---------- symptom marquee ---------- */}
      <div className="border-y border-edge-soft bg-surface/60 overflow-hidden py-3" aria-hidden="true">
        <div className="marquee-track flex gap-8 whitespace-nowrap w-max">
          {[...SYMPTOMS, ...SYMPTOMS].map((s, i) => (
            <span key={i} className="micro text-ink-faint inline-flex items-center gap-8">
              <span className="text-danger/70">✗</span> {s}
            </span>
          ))}
        </div>
      </div>

      {/* ---------- how it works ---------- */}
      <section id="how" className="max-w-6xl mx-auto px-5 py-24">
        <Reveal>
          <SectionLabel>HOW TO USE</SectionLabel>
          <h2 className="mt-4 font-display font-bold text-3xl sm:text-4xl tracking-tight">
            네 번만 따라 하면 됩니다
          </h2>
          <p className="mt-3 text-ink-muted max-w-2xl">
            각 단계의 실제 화면입니다. 로그인하지 않아도 흐름을 미리 볼 수 있어요.
          </p>
        </Reveal>

        <div className="mt-12 space-y-5">
          {/* 01 — pick a scenario */}
          <Reveal>
            <div className="grid lg:grid-cols-[1fr_1.05fr] gap-6 lg:gap-10 items-center border border-edge bg-surface p-6">
              <div>
                <div className="flex items-center gap-3">
                  <span className="font-display font-bold text-2xl text-edge select-none">01</span>
                  <h3 className="font-display font-semibold text-lg">시나리오 고르기</h3>
                </div>
                <p className="mt-2 text-sm text-ink-muted leading-relaxed">
                  문제 목록에서 카테고리·난이도(SEV-3/2/1)로 좁히거나 검색합니다.
                  푼 문제는 SOLVED 스탬프가 찍혀 한눈에 구분됩니다.
                </p>
              </div>
              <div className="border border-edge bg-canvas p-4">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-1.5"><SevTag difficulty="easy" /><TypeTag type="fix" /></div>
                  <SolveStamp state="open" />
                </div>
                <p className="mt-3 font-display font-semibold text-sm">CrashLoopBackOff 해결</p>
                <p className="mt-1 micro text-ink-faint">INC-142 · POD · 30MIN</p>
              </div>
            </div>
          </Reveal>

          {/* 02 — start environment */}
          <Reveal delay={80}>
            <div className="grid lg:grid-cols-[1fr_1.05fr] gap-6 lg:gap-10 items-center border border-edge bg-surface p-6">
              <div>
                <div className="flex items-center gap-3">
                  <span className="font-display font-bold text-2xl text-edge select-none">02</span>
                  <h3 className="font-display font-semibold text-lg">환경 시작</h3>
                </div>
                <p className="mt-2 text-sm text-ink-muted leading-relaxed">
                  <strong className="text-ink">환경 시작</strong>을 누르면 격리된 k3s 클러스터가 뜹니다.
                  부팅 → 시나리오 주입 → 대기 단계를 타임라인으로 보여주며, 끝나면 터미널이 열립니다.
                </p>
              </div>
              <div className="border border-edge bg-canvas p-4">
                <p className="micro text-ink-faint mb-3">ENVIRONMENT</p>
                <ol className="space-y-2.5 text-sm">
                  <li className="flex items-center gap-2.5"><span className="w-2 h-2 bg-success" aria-hidden="true" /><span className="text-ink">클러스터 부팅</span></li>
                  <li className="flex items-center gap-2.5"><span className="w-2 h-2 bg-success" aria-hidden="true" /><span className="text-ink">시나리오 주입</span></li>
                  <li className="flex items-center gap-2.5"><span className="w-2 h-2 bg-accent-hover animate-pulse-dot" aria-hidden="true" /><span className="text-accent-hover">대기</span></li>
                </ol>
              </div>
            </div>
          </Reveal>

          {/* 03 — diagnose in the terminal */}
          <Reveal delay={160}>
            <div className="grid lg:grid-cols-[1fr_1.05fr] gap-6 lg:gap-10 items-center border border-edge bg-surface p-6">
              <div>
                <div className="flex items-center gap-3">
                  <span className="font-display font-bold text-2xl text-edge select-none">03</span>
                  <h3 className="font-display font-semibold text-lg">터미널에서 진단</h3>
                </div>
                <p className="mt-2 text-sm text-ink-muted leading-relaxed">
                  웹 터미널은 실제 컨테이너의 셸입니다. <span className="font-mono text-info">kubectl</span>로
                  증상을 추적하고 원인을 직접 고칩니다.
                </p>
              </div>
              <div className="border border-edge bg-terminal p-4 font-mono text-xs leading-relaxed">
                <p><span className="text-accent-hover select-none">$ </span>kubectl get pods</p>
                <p className="text-danger">web-app-5b687cff65-x2k9   0/1   CrashLoopBackOff</p>
                <p className="mt-1"><span className="text-accent-hover select-none">$ </span>kubectl describe pod web-app-…</p>
                <p className="text-ink-faint">Warning  Failed  configmap "app-config" not found</p>
              </div>
            </div>
          </Reveal>

          {/* 04 — verify */}
          <Reveal delay={240}>
            <div className="grid lg:grid-cols-[1fr_1.05fr] gap-6 lg:gap-10 items-center border border-edge bg-surface p-6">
              <div>
                <div className="flex items-center gap-3">
                  <span className="font-display font-bold text-2xl text-edge select-none">04</span>
                  <h3 className="font-display font-semibold text-lg">검증으로 통과 확인</h3>
                </div>
                <p className="mt-2 text-sm text-ink-muted leading-relaxed">
                  <strong className="text-ink">검증</strong> 버튼이 판정 스크립트를 실행해 결과를 보여줍니다.
                  FIND 시나리오는 원인을 선택지로 골라 제출합니다.
                </p>
              </div>
              <div className="border border-success/40 bg-terminal">
                <div className="px-3 py-2 border-b border-success/30 micro text-success">VERIFICATION PASSED</div>
                <p className="p-3 font-mono text-xs text-success">✓ Pod is Running and Ready</p>
              </div>
            </div>
          </Reveal>
        </div>

        <Reveal delay={120}>
          <div className="mt-8 flex flex-wrap items-center gap-4">
            <Link to="/docs" className="btn-ghost">전체 사용법 보기</Link>
            <span className="micro text-ink-faint">단축키 · 용어 · 문제 제작법은 문서에서</span>
          </div>
        </Reveal>
      </section>

      {/* ---------- scenarios ---------- */}
      <section id="scenarios" className="border-t border-edge-soft bg-surface/40">
        <div className="max-w-6xl mx-auto px-5 py-24">
          <Reveal>
            <div className="flex flex-wrap items-end justify-between gap-4">
              <div>
                <SectionLabel>SCENARIO CATALOG</SectionLabel>
                <h2 className="mt-4 font-display font-bold text-3xl sm:text-4xl tracking-tight">
                  대기 중인 인시던트
                </h2>
              </div>
              <p className="micro text-ink-faint">7 SCENARIOS · 격리된 k3s 컨테이너에서 실행</p>
            </div>
          </Reveal>

          <div className="mt-12 grid md:grid-cols-2 gap-5">
            {TEASERS.map((t, i) => {
              const cat = categoryMeta[t.category]
              const CatIcon = cat?.icon
              return (
                <Reveal key={t.id} delay={i * 80}>
                  <article className="group relative border border-edge bg-surface p-6 transition-all duration-200 hover:border-accent/60 hover:-translate-y-0.5 hover:shadow-[0_16px_48px_-16px_rgba(50,108,229,0.3)]">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <SevTag difficulty={t.difficulty} />
                        <TypeTag type={t.type} />
                      </div>
                      <span className="micro text-ink-faint">{t.code}</span>
                    </div>
                    <h3 className="mt-4 font-display font-semibold text-xl flex items-center gap-2">
                      {CatIcon && <CatIcon className="w-5 h-5 text-accent-hover" aria-hidden="true" />}
                      {t.title}
                    </h3>
                    <p className="mt-2 text-sm text-ink-muted leading-relaxed">{t.desc}</p>
                    <div className="mt-5 flex items-center justify-between">
                      <span className="micro text-ink-faint">{cat?.label} · 제한 {t.timeout}분</span>
                      <span className="micro text-accent-hover opacity-0 group-hover:opacity-100 transition-opacity inline-flex items-center gap-1">
                        로그인 후 시작 <ChevronRight className="w-3.5 h-3.5" aria-hidden="true" />
                      </span>
                    </div>
                    <span className="absolute left-0 top-0 bottom-0 w-0.5 bg-accent scale-y-0 group-hover:scale-y-100 origin-top transition-transform duration-200" aria-hidden="true" />
                  </article>
                </Reveal>
              )
            })}
          </div>

          <Reveal delay={120}>
            <div className="mt-10 text-center">
              {user ? (
                <Link to="/problems" className="btn-ghost">전체 카탈로그 열기 <ArrowRight className="w-4 h-4" aria-hidden="true" /></Link>
              ) : (
                <a href="/api/auth/github" className="btn-ghost">
                  <GithubMark className="w-4 h-4" /> 로그인하고 전체 카탈로그 보기
                </a>
              )}
            </div>
          </Reveal>
        </div>
      </section>

      {/* ---------- subsystems ---------- */}
      <section className="max-w-6xl mx-auto px-5 py-24">
        <Reveal>
          <SectionLabel>SUBSYSTEMS</SectionLabel>
          <h2 className="mt-4 font-display font-bold text-3xl sm:text-4xl tracking-tight">어디가 고장 나도 이상하지 않다</h2>
        </Reveal>
        <div className="mt-10 grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-px bg-edge-soft border border-edge-soft">
          {SUBSYSTEMS.map((s, i) => {
            const meta = categoryMeta[s.key]
            const Icon = meta?.icon
            return (
              <Reveal key={s.key} delay={i * 60}>
                <div className="bg-canvas p-5 hover:bg-surface transition-colors group cursor-default">
                  {Icon && <Icon className="w-6 h-6 text-ink-faint group-hover:text-accent-hover transition-colors" aria-hidden="true" />}
                  <p className="mt-4 font-display font-semibold">{meta?.label}</p>
                  <p className="micro text-ink-faint mt-1">{s.count} scenario{s.count > 1 ? 's' : ''}</p>
                </div>
              </Reveal>
            )
          })}
        </div>
      </section>

      {/* ---------- CTA ---------- */}
      <section className="border-t border-edge-soft">
        <div className="max-w-6xl mx-auto px-5 py-24 text-center relative overflow-hidden">
          <div className="absolute inset-0 bg-[radial-gradient(ellipse_at_center,rgba(50,108,229,0.12),transparent_65%)]" aria-hidden="true" />
          <Reveal className="relative">
            <p className="micro text-accent-hover">$ kubectl apply -f your-skills.yaml</p>
            <h2 className="mt-4 font-display font-bold text-3xl sm:text-5xl tracking-tight">
              첫 클러스터를 깨뜨릴 준비가 됐나요?
            </h2>
            <p className="mt-4 text-ink-muted max-w-lg mx-auto">
              GitHub 계정만 있으면 됩니다. 첫 시나리오는 5분이면 클리어할 수 있어요.
            </p>
            <div className="mt-8">
              {user ? (
                <Link to="/" className="btn-primary text-base px-8 py-3">콘솔로 이동 <ArrowRight className="w-4 h-4" aria-hidden="true" /></Link>
              ) : (
                <a href="/api/auth/github" className="btn-primary text-base px-8 py-3">
                  <GithubMark className="w-5 h-5" /> GitHub로 시작하기
                </a>
              )}
            </div>
          </Reveal>
        </div>
      </section>

      {/* ---------- footer ---------- */}
      <footer className="border-t border-edge-soft">
        <div className="max-w-6xl mx-auto px-5 py-10 flex flex-col sm:flex-row items-center justify-between gap-4">
          <div className="flex items-center gap-2.5 font-display font-bold">
            <span className="w-6 h-6 bg-accent text-white flex items-center justify-center text-sm" aria-hidden="true">⎈</span>
            K8S<span className="text-accent-hover -ml-1.5">QUIZ</span>
          </div>
          <p className="micro text-ink-faint">KUBERNETES TROUBLESHOOTING RANGE · K3S-IN-DOCKER · VERIFIED BY SCRIPTS</p>
          <div className="flex items-center gap-5 text-sm">
            <Link to="/docs" className="text-ink-faint hover:text-ink transition-colors">문서</Link>
            <Link to={user ? '/leaderboard' : '/login'} className="text-ink-faint hover:text-ink transition-colors">리더보드</Link>
          </div>
        </div>
      </footer>
    </div>
  )
}
