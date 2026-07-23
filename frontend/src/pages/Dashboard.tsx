import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session, Progress, Attempt } from '../types'
import { useAuthStore } from '../stores/auth'
import { SevTag, TypeTag, SolveStamp, SolveState, categoryMeta, sevMeta } from '../components/ui'
import { Search, ChevronRight } from 'lucide-react'

const DIFFS = ['easy', 'medium', 'hard'] as const

function codeFor(id: string): string {
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) % 900
  return `INC-${100 + h}`
}

const DAY = 86_400_000
const ACTIVITY_DAYS = 14

/* last-N-days attempt buckets, aggregated client-side from the attempt log */
function useActivity(attempts: Attempt[]) {
  return useMemo(() => {
    const today = new Date(); today.setHours(0, 0, 0, 0)
    const buckets = Array.from({ length: ACTIVITY_DAYS }, (_, i) => ({
      date: new Date(today.getTime() - (ACTIVITY_DAYS - 1 - i) * DAY),
      ok: 0, fail: 0,
    }))
    for (const a of attempts) {
      const d = new Date(a.started_at); d.setHours(0, 0, 0, 0)
      const diff = Math.round((today.getTime() - d.getTime()) / DAY)
      if (diff >= 0 && diff < ACTIVITY_DAYS) {
        const b = buckets[ACTIVITY_DAYS - 1 - diff]
        if (a.status === 'success') b.ok++; else b.fail++
      }
    }
    return buckets
  }, [attempts])
}

function ActivityChart({ attempts }: { attempts: Attempt[] }) {
  const buckets = useActivity(attempts)
  const max = Math.max(1, ...buckets.map((b) => b.ok + b.fail))
  const total = buckets.reduce((s, b) => s + b.ok + b.fail, 0)
  return (
    <div className="border border-edge bg-surface p-5">
      <div className="flex items-baseline justify-between">
        <h2 className="font-display font-semibold text-sm">최근 {ACTIVITY_DAYS}일 활동</h2>
        <span className="font-mono text-xs text-ink-faint">{total} attempts</span>
      </div>
      {total === 0 ? (
        <div className="mt-6 flex items-end gap-1 h-24">
          {buckets.map((b, i) => (
            <div key={i} className="flex-1 border-b border-edge-soft h-px" />
          ))}
        </div>
      ) : (
        <div className="mt-6 flex items-end gap-1 h-24">
          {buckets.map((b, i) => {
            const h = ((b.ok + b.fail) / max) * 100
            const okRatio = b.ok + b.fail > 0 ? (b.ok / (b.ok + b.fail)) * 100 : 0
            return (
              <div key={i} className="flex-1 flex flex-col justify-end h-full group" title={`${b.date.getMonth() + 1}/${b.date.getDate()} · ok ${b.ok} / fail ${b.fail}`}>
                <div className="w-full flex flex-col justify-end" style={{ height: `${h}%`, minHeight: b.ok + b.fail > 0 ? 3 : 0 }}>
                  <div className="w-full bg-success/80" style={{ height: `${okRatio}%` }} />
                  <div className="w-full bg-danger/70" style={{ height: `${100 - okRatio}%` }} />
                </div>
              </div>
            )
          })}
        </div>
      )}
      <div className="mt-3 flex items-center gap-4 font-mono text-[10px] text-ink-faint">
        <span className="inline-flex items-center gap-1.5"><span className="w-2 h-2 bg-success/80" /> solved</span>
        <span className="inline-flex items-center gap-1.5"><span className="w-2 h-2 bg-danger/70" /> failed</span>
        <span className="ml-auto">{buckets[0].date.getMonth() + 1}/{buckets[0].date.getDate()} — {buckets[ACTIVITY_DAYS - 1].date.getMonth() + 1}/{buckets[ACTIVITY_DAYS - 1].date.getDate()}</span>
      </div>
    </div>
  )
}

function DifficultyBreakdown({ problems, solved }: { problems: Problem[]; solved: Record<string, boolean> }) {
  const rows = DIFFS.map((d) => {
    const all = problems.filter((p) => p.difficulty === d)
    const done = all.filter((p) => solved[p.id]).length
    return { d, total: all.length, done }
  })
  return (
    <div className="border border-edge bg-surface p-5">
      <h2 className="font-display font-semibold text-sm">난이도별 클리어</h2>
      <div className="mt-5 space-y-4">
        {rows.map((r) => {
          const m = sevMeta[r.d]
          const pct = r.total > 0 ? Math.round((r.done / r.total) * 100) : 0
          return (
            <div key={r.d}>
              <div className="flex items-center justify-between font-mono text-xs mb-1.5">
                <span className={m.cls.split(' ')[0]}>{m.sev}</span>
                <span className="text-ink-muted">{r.done}/{r.total}</span>
              </div>
              <div className="h-1.5 bg-surface-3">
                <div className={`h-full ${m.dot}`} style={{ width: `${pct}%` }} />
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}

function Stat({ value, label }: { value: React.ReactNode; label: string }) {
  return (
    <div className="px-5 first:pl-0 last:pr-0 border-l border-edge-soft first:border-l-0 first:pl-0">
      <div className="font-display font-bold text-2xl leading-none tabular-nums">{value}</div>
      <div className="font-mono text-[10px] uppercase tracking-wider text-ink-faint mt-1.5">{label}</div>
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
      className={`group relative flex flex-col border bg-surface p-4 transition-colors ${
        solved ? 'border-edge-soft opacity-70 hover:opacity-100' : 'border-edge hover:border-accent/60'
      }`}
    >
      <span className={`absolute left-0 top-0 bottom-0 w-0.5 ${solved ? 'bg-success' : 'bg-transparent group-hover:bg-accent'}`} aria-hidden="true" />
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5">
          <SevTag difficulty={p.difficulty} />
          <TypeTag type={p.type} />
        </div>
        <SolveStamp state={state} />
      </div>
      <h3 className="mt-3 font-display font-semibold text-[15px] leading-snug flex items-center gap-2">
        {CatIcon && <CatIcon className="w-4 h-4 text-ink-faint shrink-0" aria-hidden="true" />}
        {p.title}
      </h3>
      <div className="mt-3 flex items-center justify-between font-mono text-[10px] text-ink-faint">
        <span>{codeFor(p.id)} · {cat?.label || p.category}</span>
        <span className="inline-flex items-center gap-0.5 text-ink-faint group-hover:text-accent-hover">
          {p.timeout_minutes}m <ChevronRight className="w-3 h-3" aria-hidden="true" />
        </span>
      </div>
    </Link>
  )
}

export function Dashboard() {
  const { user } = useAuthStore()
  const [problems, setProblems] = useState<Problem[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [attempts, setAttempts] = useState<Attempt[]>([])
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
      api.get<{ attempts: Attempt[] }>('/api/users/me/attempts'),
      api.get<Session>('/api/sessions/current'),
    ]).then(([p, pr, at, s]) => {
      if (!alive) return
      if (p.status === 'fulfilled') setProblems(p.value.problems)
      if (pr.status === 'fulfilled') setProgress(pr.value)
      if (at.status === 'fulfilled') setAttempts(at.value.attempts)
      setActiveSession(s.status === 'fulfilled' ? s.value : null)
      setLoading(false)
    })
    return () => { alive = false }
  }, [])

  const cats = useMemo(() => Array.from(new Set(problems.map((p) => p.category))), [problems])
  const solvedMap = progress?.solved ?? {}
  const solvedCount = progress?.solved_count ?? 0
  const total = problems.length
  const open = total - solvedCount
  const pct = total > 0 ? Math.round((solvedCount / total) * 100) : 0

  const order = { easy: 0, medium: 1, hard: 2 } as Record<string, number>
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return problems
      .filter((p) => (category ? p.category === category : true))
      .filter((p) => (diff ? p.difficulty === diff : true))
      .filter((p) => (q ? p.title.toLowerCase().includes(q) || p.id.includes(q) : true))
      .sort((a, b) => {
        const sa = solvedMap[a.id] ? 1 : 0
        const sb = solvedMap[b.id] ? 1 : 0
        if (sa !== sb) return sa - sb
        return (order[a.difficulty] ?? 9) - (order[b.difficulty] ?? 9)
      })
  }, [problems, solvedMap, query, category, diff])

  return (
    <div className="max-w-7xl mx-auto px-5 py-8 space-y-6">
      {/* ---- identity + headline stats (no greeting, no copy) ---- */}
      <header className="border border-edge bg-surface px-5 py-4 flex flex-wrap items-center gap-x-8 gap-y-4">
        <div className="flex items-center gap-3">
          {user?.avatar_url ? (
            <img src={user.avatar_url} alt="" className="w-10 h-10 rounded-full border border-edge" />
          ) : (
            <span className="w-10 h-10 rounded-full border border-edge bg-surface-2 flex items-center justify-center font-display font-bold text-ink-faint">
              {user?.username?.[0]?.toUpperCase()}
            </span>
          )}
          <div className="leading-tight">
            <div className="font-display font-semibold text-base">{user?.username}</div>
            <div className="font-mono text-[10px] uppercase tracking-wider text-ink-faint">{user?.role}</div>
          </div>
        </div>
        <div className="flex items-center ml-auto">
          <Stat value={solvedCount} label="solved" />
          <Stat value={progress?.total_attempts ?? 0} label="attempts" />
          <Stat value={`${pct}%`} label="clearance" />
          <Stat value={open} label="open" />
        </div>
      </header>

      {/* ---- active session: one quiet line, no marketing copy ---- */}
      {activeSession && (
        <Link
          to={`/problems/${activeSession.problem_id}`}
          className="flex items-center gap-3 border border-accent/40 bg-accent-dim px-4 py-2.5 text-sm hover:bg-accent-soft transition-colors"
        >
          <span className="w-1.5 h-1.5 bg-accent-hover" aria-hidden="true" />
          <span className="text-ink-muted">in progress</span>
          <span className="font-mono text-accent-hover">{activeSession.problem_id}</span>
          <ChevronRight className="w-4 h-4 ml-auto text-accent-hover" aria-hidden="true" />
        </Link>
      )}

      {/* ---- charts: activity + difficulty (data, not description) ---- */}
      <div className="grid lg:grid-cols-3 gap-4">
        <div className="lg:col-span-2"><ActivityChart attempts={attempts} /></div>
        <DifficultyBreakdown problems={problems} solved={solvedMap} />
      </div>

      {/* ---- scenario list: filters inline, no section blurb ---- */}
      <section>
        <div className="flex flex-wrap items-center gap-3 border-b border-edge pb-3">
          <h2 className="font-display font-semibold text-sm mr-1">
            시나리오 <span className="text-ink-faint font-mono font-normal">{filtered.length}/{total}</span>
          </h2>
          <div className="relative ml-auto w-full sm:w-56">
            <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-ink-faint" aria-hidden="true" />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="검색"
              aria-label="시나리오 검색"
              className="w-full bg-surface border border-edge pl-8 pr-2 py-1.5 text-xs placeholder:text-ink-faint focus:border-accent/60 focus:outline-none"
            />
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-1.5 py-3">
          {[['', '전체'], ...cats.map((c) => [c, categoryMeta[c]?.label || c] as [string, string])].map(([key, label]) => (
            <button
              key={key}
              onClick={() => setCategory(key)}
              aria-pressed={category === key}
              className={`px-2.5 py-1 text-xs font-mono border transition-colors ${
                category === key ? 'border-accent/60 bg-accent-soft text-accent-hover' : 'border-edge text-ink-faint hover:text-ink-muted'
              }`}
            >
              {label}
            </button>
          ))}
          <span className="w-px h-4 bg-edge-soft mx-1" aria-hidden="true" />
          {DIFFS.map((d) => (
            <button
              key={d}
              onClick={() => setDiff(diff === d ? '' : d)}
              aria-pressed={diff === d}
              className={`px-2 py-1 text-[10px] font-mono border transition-colors ${
                diff === d ? sevMeta[d].cls + ' border-current' : 'border-edge text-ink-faint hover:text-ink-muted'
              }`}
            >
              {sevMeta[d].sev}
            </button>
          ))}
        </div>

        {loading ? (
          <div className="grid sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3">
            {Array.from({ length: 8 }).map((_, i) => (
              <div key={i} className="border border-edge-soft bg-surface p-4 h-28 animate-pulse">
                <div className="h-3 w-20 bg-surface-3" />
                <div className="mt-3 h-4 w-3/4 bg-surface-3" />
              </div>
            ))}
          </div>
        ) : filtered.length === 0 ? (
          <div className="border border-edge-soft bg-surface py-12 text-center font-mono text-xs text-ink-faint">
            no scenarios match
          </div>
        ) : (
          <div className="grid sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3">
            {filtered.map((p) => (
              <TicketCard key={p.id} p={p} state={solvedMap[p.id] ? 'solved' : 'open'} />
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
