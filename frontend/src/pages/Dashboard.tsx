import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session, Progress, Attempt, Achievement } from '../types'
import { useAuthStore } from '../stores/auth'
import { sevMeta } from '../components/ui'
import { Award, Lock, ChevronRight } from 'lucide-react'

const DAY = 86_400_000
const ACTIVITY_DAYS = 14

const statusMeta: Record<string, { label: string; cls: string }> = {
  success:     { label: 'SOLVED', cls: 'text-success' },
  failed:      { label: 'FAILED', cls: 'text-danger' },
  in_progress: { label: 'OPEN',   cls: 'text-warning' },
  timeout:     { label: 'TIMEOUT',cls: 'text-warning' },
}

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
          {buckets.map((_, i) => <div key={i} className="flex-1 border-b border-edge-soft h-px" />)}
        </div>
      ) : (
        <div className="mt-6 flex items-end gap-1 h-24">
          {buckets.map((b, i) => {
            const h = ((b.ok + b.fail) / max) * 100
            const okRatio = b.ok + b.fail > 0 ? (b.ok / (b.ok + b.fail)) * 100 : 0
            return (
              <div key={i} className="flex-1 flex flex-col justify-end h-full" title={`${b.date.getMonth() + 1}/${b.date.getDate()} · ok ${b.ok} / fail ${b.fail}`}>
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
  const rows = (['easy', 'medium', 'hard'] as const).map((d) => {
    const all = problems.filter((p) => p.difficulty === d)
    return { d, total: all.length, done: all.filter((p) => solved[p.id]).length }
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
    <div className="px-5 border-l border-edge-soft first:border-l-0 first:pl-0">
      <div className="font-display font-bold text-2xl leading-none tabular-nums">{value}</div>
      <div className="font-mono text-[10px] uppercase tracking-wider text-ink-faint mt-1.5">{label}</div>
    </div>
  )
}

export function Dashboard() {
  const { user } = useAuthStore()
  const [problems, setProblems] = useState<Problem[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [attempts, setAttempts] = useState<Attempt[]>([])
  const [achievements, setAchievements] = useState<Achievement[]>([])
  const [activeSession, setActiveSession] = useState<Session | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let alive = true
    Promise.allSettled([
      api.get<{ problems: Problem[] }>('/api/problems'),
      api.get<Progress>('/api/users/me/progress'),
      api.get<{ attempts: Attempt[] }>('/api/users/me/attempts'),
      api.get<{ achievements: Achievement[] }>('/api/users/me/achievements'),
      api.get<Session>('/api/sessions/current'),
    ]).then(([p, pr, at, ac, s]) => {
      if (!alive) return
      if (p.status === 'fulfilled') setProblems(p.value.problems)
      if (pr.status === 'fulfilled') setProgress(pr.value)
      if (at.status === 'fulfilled') setAttempts(at.value.attempts)
      if (ac.status === 'fulfilled') setAchievements(ac.value.achievements)
      setActiveSession(s.status === 'fulfilled' ? s.value : null)
      setLoading(false)
    })
    return () => { alive = false }
  }, [])

  const solvedMap = progress?.solved ?? {}
  const solvedCount = progress?.solved_count ?? 0
  const total = problems.length
  const pct = total > 0 ? Math.round((solvedCount / total) * 100) : 0
  const unlocked = achievements.filter((a) => a.unlocked).length

  const recent = useMemo(
    () => [...attempts].sort((a, b) => +new Date(b.started_at) - +new Date(a.started_at)).slice(0, 6),
    [attempts]
  )

  return (
    <div className="max-w-7xl mx-auto px-5 py-8 space-y-6">
      {/* identity + headline stats */}
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
          <Stat value={total - solvedCount} label="open" />
        </div>
      </header>

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

      {/* charts */}
      <div className="grid lg:grid-cols-3 gap-4">
        <div className="lg:col-span-2"><ActivityChart attempts={attempts} /></div>
        <DifficultyBreakdown problems={problems} solved={solvedMap} />
      </div>

      {/* achievements + recent solves */}
      <div className="grid lg:grid-cols-3 gap-4">
        <div className="border border-edge bg-surface p-5 lg:col-span-2">
          <div className="flex items-baseline justify-between">
            <h2 className="font-display font-semibold text-sm">최근 시도</h2>
            <Link to="/profile" className="font-mono text-[10px] text-ink-faint hover:text-accent-hover inline-flex items-center gap-0.5">
              전체 <ChevronRight className="w-3 h-3" aria-hidden="true" />
            </Link>
          </div>
          {recent.length === 0 ? (
            <p className="mt-6 font-mono text-xs text-ink-faint">아직 시도 기록이 없습니다.</p>
          ) : (
            <div className="mt-4 divide-y divide-edge-soft">
              {recent.map((a) => {
                const m = statusMeta[a.status] || statusMeta.in_progress
                return (
                  <Link key={a.id} to={`/problems/${a.problem_id}`} className="flex items-center gap-3 py-2 hover:bg-surface-2 -mx-2 px-2 transition-colors">
                    <span className={`font-mono text-[10px] w-14 ${m.cls}`}>{m.label}</span>
                    <span className="font-mono text-xs text-ink truncate">{a.problem_id}</span>
                    <span className="ml-auto font-mono text-[10px] text-ink-faint shrink-0">
                      {a.duration_seconds != null ? `${Math.floor(a.duration_seconds / 60)}m ${a.duration_seconds % 60}s` : '—'}
                    </span>
                    <span className="font-mono text-[10px] text-ink-faint shrink-0 w-16 text-right">
                      {new Date(a.started_at).toLocaleDateString()}
                    </span>
                  </Link>
                )
              })}
            </div>
          )}
        </div>

        <div className="border border-edge bg-surface p-5">
          <div className="flex items-baseline justify-between">
            <h2 className="font-display font-semibold text-sm">배지</h2>
            <span className="font-mono text-[10px] text-ink-faint">{unlocked}/{achievements.length}</span>
          </div>
          <div className="mt-4 grid grid-cols-2 gap-2">
            {achievements.map((a) => (
              <div
                key={a.id}
                title={a.description}
                className={`flex items-center gap-2 border p-2 ${a.unlocked ? 'border-success/40 bg-success-soft' : 'border-edge-soft opacity-50'}`}
              >
                {a.unlocked
                  ? <Award className="w-4 h-4 text-success shrink-0" aria-hidden="true" />
                  : <Lock className="w-4 h-4 text-ink-faint shrink-0" aria-hidden="true" />}
                <span className="text-[11px] leading-tight truncate">{a.title}</span>
              </div>
            ))}
          </div>
        </div>
      </div>

      {loading && <div className="font-mono text-[10px] text-ink-faint">loading…</div>}
    </div>
  )
}
