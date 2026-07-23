import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session, Progress } from '../types'
import { useAuthStore } from '../stores/auth'
import { SectionLabel, SevTag, TypeTag, SolveStamp, SolveState, categoryMeta, sevMeta } from '../components/ui'
import { Search, PlayCircle, ChevronRight, Activity, TerminalSquare, Crosshair, Layers } from 'lucide-react'

const DIFFS = ['easy', 'medium', 'hard'] as const

function codeFor(id: string): string {
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) % 900
  return `INC-${100 + h}`
}

/* circular clearance gauge */
function Ring({ pct, solved, total }: { pct: number; solved: number; total: number }) {
  const r = 42
  const c = 2 * Math.PI * r
  const off = c * (1 - Math.min(100, pct) / 100)
  return (
    <div className="relative w-36 h-36 shrink-0">
      <svg viewBox="0 0 100 100" className="w-full h-full -rotate-90">
        <circle cx="50" cy="50" r={r} fill="none" stroke="#1c2432" strokeWidth="7" />
        <circle
          cx="50" cy="50" r={r} fill="none" stroke="#326ce5" strokeWidth="7"
          strokeDasharray={c} strokeDashoffset={off} strokeLinecap="square"
          style={{ transition: 'stroke-dashoffset .9s cubic-bezier(.2,.8,.2,1)', filter: 'drop-shadow(0 0 6px rgba(50,108,229,.6))' }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="font-display font-bold text-3xl leading-none">{pct}%</span>
        <span className="micro text-ink-faint mt-1">{solved}/{total} CLEARED</span>
      </div>
    </div>
  )
}

function TicketCard({ p, state }: { p: Problem; state: SolveState }) {
  const cat = categoryMeta[p.category]
  const CatIcon = cat?.icon
  const solved = state === 'solved'
  return (
    <Link
      to={`/problems/${p.id}`}
      className={`group relative flex flex-col border bg-surface p-5 transition-all duration-200 hover:-translate-y-0.5 ${
        solved
          ? 'border-edge-soft opacity-75 hover:opacity-100 hover:border-success/40'
          : 'border-edge hover:border-accent/60 hover:shadow-[0_16px_48px_-16px_rgba(50,108,229,0.3)]'
      }`}
    >
      <span
        className={`absolute left-0 top-0 bottom-0 w-0.5 origin-top transition-transform duration-200 ${
          solved ? 'bg-success' : 'bg-accent scale-y-0 group-hover:scale-y-100'
        }`}
        aria-hidden="true"
      />
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 flex-wrap">
          <SevTag difficulty={p.difficulty} />
          <TypeTag type={p.type} />
        </div>
        <SolveStamp state={state} />
      </div>
      <h3 className="mt-4 font-display font-semibold text-lg leading-snug flex items-start gap-2">
        {CatIcon && <CatIcon className="w-4 h-4 mt-1 text-accent-hover shrink-0" aria-hidden="true" />}
        {p.title}
      </h3>
      <p className="mt-2 text-sm text-ink-muted leading-relaxed line-clamp-2 flex-1">{p.description}</p>
      <div className="mt-4 flex items-center justify-between">
        <span className="micro text-ink-faint">{codeFor(p.id)} · {cat?.label || p.category} · {p.timeout_minutes}MIN</span>
        <span className={`micro inline-flex items-center gap-1 transition-colors ${solved ? 'text-success' : 'text-accent-hover opacity-0 group-hover:opacity-100'}`}>
          {solved ? '다시 풀기' : '시작'}
          <ChevronRight className="w-3.5 h-3.5" aria-hidden="true" />
        </span>
      </div>
    </Link>
  )
}

export function Dashboard() {
  const { user } = useAuthStore()
  const [problems, setProblems] = useState<Problem[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [activeSession, setActiveSession] = useState<Session | null>(null)
  const [loading, setLoading] = useState(true)
  const [query, setQuery] = useState('')
  const [category, setCategory] = useState('')
  const [diff, setDiff] = useState('')

  useEffect(() => {
    let alive = true
    Promise.allSettled([
      api.get<{ problems: Problem[] }>('/api/problems'),
      api.get<Progress>('/api/users/me/progress'),
      api.get<Session>('/api/sessions/current'),
    ]).then(([p, pr, s]) => {
      if (!alive) return
      if (p.status === 'fulfilled') setProblems(p.value.problems)
      if (pr.status === 'fulfilled') setProgress(pr.value)
      setActiveSession(s.status === 'fulfilled' ? s.value : null)
      setLoading(false)
    })
    return () => { alive = false }
  }, [])

  const cats = useMemo(() => Array.from(new Set(problems.map((p) => p.category))), [problems])

  const solvedCount = progress?.solved_count ?? 0
  const total = problems.length
  const pct = total > 0 ? Math.round((solvedCount / total) * 100) : 0

  // featured = easiest unsolved scenario (falls back to easiest overall)
  const order = { easy: 0, medium: 1, hard: 2 } as Record<string, number>
  const featured = useMemo(() => {
    const sorted = [...problems].sort((a, b) => (order[a.difficulty] ?? 9) - (order[b.difficulty] ?? 9))
    return sorted.find((p) => !progress?.solved[p.id]) || sorted[0] || null
  }, [problems, progress])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return problems
      .filter((p) => (category ? p.category === category : true))
      .filter((p) => (diff ? p.difficulty === diff : true))
      .filter((p) => (q ? p.title.toLowerCase().includes(q) || p.description.toLowerCase().includes(q) || p.id.includes(q) : true))
      .sort((a, b) => {
        const sa = progress?.solved[a.id] ? 1 : 0
        const sb = progress?.solved[b.id] ? 1 : 0
        if (sa !== sb) return sa - sb
        return (order[a.difficulty] ?? 9) - (order[b.difficulty] ?? 9)
      })
  }, [problems, progress, query, category, diff])

  return (
    <div className="max-w-7xl mx-auto px-5 py-8">
      {/* ===================== HERO ===================== */}
      <section className="relative overflow-hidden border border-edge bg-surface">
        <div className="absolute inset-0 bg-grid opacity-60" aria-hidden="true" />
        <div className="absolute -right-24 -top-24 w-72 h-72 bg-[radial-gradient(circle,rgba(50,108,229,0.18),transparent_70%)]" aria-hidden="true" />
        <div className="relative grid lg:grid-cols-[1fr_auto] gap-8 items-center p-8 sm:p-10">
          <div>
            <SectionLabel>OPERATOR CONSOLE</SectionLabel>
            <h1 className="mt-3 font-display font-bold text-3xl sm:text-[40px] leading-[1.1] tracking-tight">
              다시 오셨군요, <span className="text-accent-hover">{user?.username}</span>.
              <br className="hidden sm:block" />
              <span className="text-ink-muted text-2xl sm:text-3xl">깨진 클러스터가 당신을 기다립니다.</span>
            </h1>
            <p className="mt-4 text-sm text-ink-muted max-w-xl leading-relaxed">
              시나리오를 골라 환경을 띄우고, 웹 터미널에서 <span className="font-mono text-info">kubectl</span>로 원인을 진단하세요.
              검증 스크립트가 당신의 실력을 판정합니다.
            </p>

            <div className="mt-7 flex flex-wrap gap-3">
              {activeSession ? (
                <Link to={`/problems/${activeSession.problem_id}`} className="btn-primary">
                  <PlayCircle className="w-4 h-4" aria-hidden="true" /> 진행중 세션 복귀
                </Link>
              ) : featured ? (
                <Link to={`/problems/${featured.id}`} className="btn-primary">
                  <Crosshair className="w-4 h-4" aria-hidden="true" /> 다음 시나리오 시작
                </Link>
              ) : null}
              <a href="#catalog" className="btn-ghost"><Layers className="w-4 h-4" aria-hidden="true" /> 전체 카탈로그</a>
            </div>
          </div>

          {/* clearance gauge + mini stats */}
          <div className="flex items-center gap-6">
            <Ring pct={pct} solved={solvedCount} total={total} />
            <div className="hidden sm:flex flex-col gap-4">
              <div className="border border-edge-soft bg-canvas/60 px-4 py-3 min-w-[120px]">
                <p className="micro text-ink-faint">ATTEMPTS</p>
                <p className="font-display font-bold text-2xl">{progress?.total_attempts ?? 0}</p>
              </div>
              <div className="border border-edge-soft bg-canvas/60 px-4 py-3">
                <p className="micro text-ink-faint">STATUS</p>
                <p className="micro text-success mt-1 inline-flex items-center gap-1.5">
                  <span className="w-1.5 h-1.5 bg-success animate-pulse-dot" aria-hidden="true" /> RANGE ONLINE
                </p>
              </div>
            </div>
          </div>
        </div>

        {/* active session strip */}
        {activeSession && (
          <Link
            to={`/problems/${activeSession.problem_id}`}
            className="relative flex items-center gap-3 px-8 sm:px-10 py-3.5 border-t border-accent/30 bg-accent-dim hover:bg-accent-soft transition-colors group"
          >
            <PlayCircle className="w-5 h-5 text-accent-hover shrink-0" aria-hidden="true" />
            <p className="text-sm">
              <span className="font-semibold">진행 중인 세션</span>
              <span className="font-mono text-accent-hover ml-2">{activeSession.problem_id}</span>
              <span className="text-ink-muted ml-2 hidden sm:inline">· 환경이 아직 살아 있습니다</span>
            </p>
            <span className="ml-auto micro text-accent-hover inline-flex items-center gap-1">
              복귀 <ChevronRight className="w-3.5 h-3.5 group-hover:translate-x-0.5 transition-transform" aria-hidden="true" />
            </span>
          </Link>
        )}
      </section>

      {/* ===================== CATEGORY QUICK-NAV ===================== */}
      <section className="mt-10">
        <SectionLabel className="mb-4">SUBSYSTEMS · 클릭하여 필터</SectionLabel>
        <div className="grid grid-cols-3 sm:grid-cols-6 gap-px bg-edge-soft border border-edge-soft">
          {cats.map((key) => {
            const meta = categoryMeta[key]
            const Icon = meta?.icon
            const n = problems.filter((p) => p.category === key).length
            const active = category === key
            return (
              <button
                key={key}
                onClick={() => { setCategory(active ? '' : key); document.getElementById('catalog')?.scrollIntoView({ behavior: 'smooth' }) }}
                className={`bg-canvas p-4 text-left transition-colors group ${active ? 'bg-accent-dim' : 'hover:bg-surface'}`}
                aria-pressed={active}
              >
                {Icon && <Icon className={`w-5 h-5 transition-colors ${active ? 'text-accent-hover' : 'text-ink-faint group-hover:text-accent-hover'}`} aria-hidden="true" />}
                <p className={`mt-3 font-display font-semibold text-sm ${active ? 'text-accent-hover' : ''}`}>{meta?.label || key}</p>
                <p className="micro text-ink-faint mt-0.5">{n}건</p>
              </button>
            )
          })}
        </div>
      </section>

      {/* ===================== CATALOG SECTION ===================== */}
      <section id="catalog" className="mt-12 scroll-mt-20">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <SectionLabel>SCENARIO CATALOG · {filtered.length}건</SectionLabel>
          <span className="micro text-ink-faint hidden sm:inline">미해결 우선 정렬</span>
        </div>

        {/* filters */}
        <div className="mt-4 flex flex-col lg:flex-row lg:items-center gap-4">
          <div className="relative flex-1 max-w-md">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-ink-faint" aria-hidden="true" />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="문제 검색…"
              aria-label="문제 검색"
              className="w-full bg-surface border border-edge pl-9 pr-3 py-2 text-sm placeholder:text-ink-faint focus:border-accent/60 focus:outline-none transition-colors"
            />
          </div>
          <div className="flex items-center gap-1 overflow-x-auto pb-1 lg:pb-0" role="tablist" aria-label="카테고리">
            {[['', 'ALL'], ...cats.map((c) => [c, categoryMeta[c]?.label || c] as [string, string])].map(([key, label]) => (
              <button
                key={key}
                role="tab"
                aria-selected={category === key}
                onClick={() => setCategory(key)}
                className={`px-3 py-1.5 text-xs font-mono tracking-wide whitespace-nowrap border transition-colors ${
                  category === key ? 'border-accent/60 bg-accent-soft text-accent-hover' : 'border-transparent text-ink-muted hover:text-ink hover:border-edge'
                }`}
              >
                {label}
              </button>
            ))}
          </div>
          <div className="flex items-center gap-1" role="group" aria-label="난이도">
            {DIFFS.map((d) => (
              <button
                key={d}
                onClick={() => setDiff(diff === d ? '' : d)}
                aria-pressed={diff === d}
                className={`px-2.5 py-1.5 text-[10px] font-mono border transition-colors ${
                  diff === d ? sevMeta[d].cls + ' border-current' : 'border-edge text-ink-faint hover:text-ink-muted'
                }`}
              >
                {sevMeta[d].sev}
              </button>
            ))}
          </div>
        </div>

        {/* grid */}
        {loading ? (
          <div className="mt-6 grid sm:grid-cols-2 xl:grid-cols-3 gap-4">
            {Array.from({ length: 6 }).map((_, i) => (
              <div key={i} className="border border-edge-soft bg-surface p-5 h-44 animate-pulse">
                <div className="h-4 w-24 bg-surface-3" />
                <div className="mt-4 h-5 w-3/4 bg-surface-3" />
                <div className="mt-3 h-3 w-full bg-surface-2" />
                <div className="mt-2 h-3 w-2/3 bg-surface-2" />
              </div>
            ))}
          </div>
        ) : filtered.length === 0 ? (
          <div className="mt-6 border border-edge-soft bg-surface py-16 text-center">
            <Activity className="w-8 h-8 text-ink-faint mx-auto" aria-hidden="true" />
            <p className="mt-3 text-ink-muted">조건에 맞는 시나리오가 없습니다.</p>
            <button onClick={() => { setQuery(''); setCategory(''); setDiff('') }} className="mt-4 micro text-accent-hover hover:underline">
              필터 초기화
            </button>
          </div>
        ) : (
          <div className="mt-6 grid sm:grid-cols-2 xl:grid-cols-3 gap-4">
            {filtered.map((p) => (
              <TicketCard key={p.id} p={p} state={progress?.solved[p.id] ? 'solved' : 'open'} />
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
