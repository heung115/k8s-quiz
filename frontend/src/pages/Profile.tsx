import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { Attempt, Progress, Achievement } from '../types'
import { useAuthStore } from '../stores/auth'
import { SectionLabel } from '../components/ui'
import { CheckCircle2, XCircle, Clock, AlertTriangle, Award, Lock } from 'lucide-react'

const statusMeta: Record<string, { label: string; cls: string; icon: typeof CheckCircle2 }> = {
  success:     { label: 'SUCCESS',     cls: 'text-success', icon: CheckCircle2 },
  failed:      { label: 'FAILED',      cls: 'text-danger',  icon: XCircle },
  in_progress: { label: 'IN PROGRESS', cls: 'text-warning', icon: Clock },
  timeout:     { label: 'TIMEOUT',     cls: 'text-warning', icon: AlertTriangle },
}

export function Profile() {
  const { user } = useAuthStore()
  const [attempts, setAttempts] = useState<Attempt[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [achievements, setAchievements] = useState<Achievement[]>([])

  useEffect(() => {
    Promise.all([
      api.get<{ attempts: Attempt[] }>('/api/users/me/attempts'),
      api.get<Progress>('/api/users/me/progress'),
      api.get<{ achievements: Achievement[] }>('/api/users/me/achievements'),
    ])
      .then(([att, prog, ach]) => {
        setAttempts(att.attempts)
        setProgress(prog)
        setAchievements(ach.achievements)
      })
      .catch(console.error)
  }, [])

  const unlocked = achievements.filter((a) => a.unlocked).length

  return (
    <div className="max-w-4xl mx-auto px-5 py-10">
      {/* header */}
      <div className="flex items-center gap-5">
        {user?.avatar_url ? (
          <img src={user.avatar_url} alt="" className="w-16 h-16 rounded-full border-2 border-edge" />
        ) : (
          <span className="w-16 h-16 rounded-full border-2 border-edge bg-surface-2 flex items-center justify-center font-display font-bold text-2xl text-ink-faint">
            {user?.username?.[0]?.toUpperCase()}
          </span>
        )}
        <div>
          <SectionLabel>OPERATOR FILE</SectionLabel>
          <h1 className="mt-1 font-display font-bold text-3xl tracking-tight">{user?.username}</h1>
          <p className="font-mono text-xs text-ink-faint mt-1">{user?.email || 'email not provided'} · joined {user && new Date(user.created_at).toLocaleDateString()}</p>
        </div>
        <div className="ml-auto hidden sm:flex gap-6 text-center">
          <div>
            <p className="font-display font-bold text-3xl text-success">{progress?.solved_count ?? 0}</p>
            <p className="micro text-ink-faint">SOLVED</p>
          </div>
          <div>
            <p className="font-display font-bold text-3xl text-accent-hover">{progress?.total_attempts ?? 0}</p>
            <p className="micro text-ink-faint">ATTEMPTS</p>
          </div>
        </div>
      </div>

      {/* achievements */}
      {achievements.length > 0 && (
        <section className="mt-12">
          <div className="flex items-baseline justify-between">
            <SectionLabel>ACHIEVEMENTS · {unlocked}/{achievements.length}</SectionLabel>
          </div>
          <div className="mt-4 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
            {achievements.map((a) => (
              <div
                key={a.id}
                className={`flex items-start gap-3 border p-4 transition-colors ${
                  a.unlocked
                    ? 'border-success/40 bg-success-soft'
                    : 'border-edge-soft bg-surface opacity-60'
                }`}
              >
                {a.unlocked
                  ? <Award className="w-5 h-5 mt-0.5 shrink-0 text-success" aria-hidden="true" />
                  : <Lock className="w-5 h-5 mt-0.5 shrink-0 text-ink-faint" aria-hidden="true" />}
                <div>
                  <p className="font-display font-semibold text-sm">{a.title}</p>
                  <p className="text-xs text-ink-muted mt-0.5 leading-relaxed">{a.description}</p>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      {/* attempt log */}
      <section className="mt-12">
        <SectionLabel>ATTEMPT LOG · {attempts.length}</SectionLabel>
        {attempts.length === 0 ? (
          <div className="mt-4 border border-edge-soft bg-surface py-12 text-center">
            <p className="text-ink-muted">아직 시도 기록이 없습니다.</p>
          </div>
        ) : (
          <div className="mt-4 border border-edge bg-surface divide-y divide-edge-soft">
            {attempts.map((a) => {
              const m = statusMeta[a.status] || statusMeta.in_progress
              const Icon = m.icon
              return (
                <div key={a.id} className="flex items-center gap-3 px-4 py-3 hover:bg-surface-2 transition-colors">
                  <Icon className={`w-4 h-4 shrink-0 ${m.cls}`} aria-hidden="true" />
                  <span className="font-mono text-sm font-medium">{a.problem_id}</span>
                  <span className={`micro ${m.cls}`}>{m.label}</span>
                  <span className="ml-auto font-mono text-xs text-ink-faint shrink-0">
                    {a.duration_seconds != null ? `${Math.floor(a.duration_seconds / 60)}m ${a.duration_seconds % 60}s` : '—'}
                  </span>
                  <span className="font-mono text-xs text-ink-faint hidden sm:inline shrink-0 w-24 text-right">
                    {new Date(a.started_at).toLocaleDateString()}
                  </span>
                </div>
              )
            })}
          </div>
        )}
      </section>
    </div>
  )
}
