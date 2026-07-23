import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Progress } from '../types'
import { SevTag, TypeTag, SolveStamp, SolveState, categoryMeta, sevMeta } from '../components/ui'
import { Search, ChevronRight } from 'lucide-react'

const DIFFS = ['easy', 'medium', 'hard'] as const

function codeFor(id: string): string {
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) % 900
  return `INC-${100 + h}`
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
      <p className="mt-2 text-xs text-ink-muted leading-relaxed line-clamp-2 flex-1">{p.description}</p>
      <div className="mt-3 flex items-center justify-between font-mono text-[10px] text-ink-faint">
        <span>{codeFor(p.id)} · {cat?.label || p.category}</span>
        <span className="inline-flex items-center gap-0.5 group-hover:text-accent-hover">
          {p.timeout_minutes}m <ChevronRight className="w-3 h-3" aria-hidden="true" />
        </span>
      </div>
    </Link>
  )
}

export function Problems() {
  const [problems, setProblems] = useState<Problem[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [loading, setLoading] = useState(true)
  const [query, setQuery] = useState('')
  const [category, setCategory] = useState('')
  const [diff, setDiff] = useState('')

  useEffect(() => {
    let alive = true
    Promise.allSettled([
      api.get<{ problems: Problem[] }>('/api/problems'),
      api.get<Progress>('/api/users/me/progress'),
    ]).then(([p, pr]) => {
      if (!alive) return
      if (p.status === 'fulfilled') setProblems(p.value.problems)
      if (pr.status === 'fulfilled') setProgress(pr.value)
      setLoading(false)
    })
    return () => { alive = false }
  }, [])

  const cats = useMemo(() => Array.from(new Set(problems.map((p) => p.category))), [problems])
  const solvedMap = progress?.solved ?? {}
  const order = { easy: 0, medium: 1, hard: 2 } as Record<string, number>

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return problems
      .filter((p) => (category ? p.category === category : true))
      .filter((p) => (diff ? p.difficulty === diff : true))
      .filter((p) => (q ? p.title.toLowerCase().includes(q) || p.id.includes(q) || p.description.toLowerCase().includes(q) : true))
      .sort((a, b) => {
        const sa = solvedMap[a.id] ? 1 : 0
        const sb = solvedMap[b.id] ? 1 : 0
        if (sa !== sb) return sa - sb
        return (order[a.difficulty] ?? 9) - (order[b.difficulty] ?? 9)
      })
  }, [problems, solvedMap, query, category, diff])

  return (
    <div className="max-w-7xl mx-auto px-5 py-8">
      <div className="flex flex-wrap items-center gap-3 border-b border-edge pb-3">
        <h1 className="font-display font-semibold text-base mr-1">
          시나리오 <span className="text-ink-faint font-mono font-normal">{filtered.length}/{problems.length}</span>
        </h1>
        <div className="relative ml-auto w-full sm:w-60">
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
            <div key={i} className="border border-edge-soft bg-surface p-4 h-32 animate-pulse">
              <div className="h-3 w-20 bg-surface-3" />
              <div className="mt-3 h-4 w-3/4 bg-surface-3" />
              <div className="mt-2 h-3 w-full bg-surface-2" />
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
    </div>
  )
}
