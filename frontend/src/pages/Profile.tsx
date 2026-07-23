import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { Attempt, Progress, Achievement } from '../types'
import { useAuthStore } from '../stores/auth'
import { CheckCircle, XCircle, Clock, AlertTriangle, Award } from 'lucide-react'

const statusConfig: Record<string, { color: string; icon: typeof CheckCircle }> = {
  success: { color: 'text-success', icon: CheckCircle },
  failed: { color: 'text-danger', icon: XCircle },
  in_progress: { color: 'text-warning', icon: Clock },
  timeout: { color: 'text-warning', icon: AlertTriangle },
}

export function Profile() {
  const { user } = useAuthStore()
  const [attempts, setAttempts] = useState<Attempt[]>([])
  const [progress, setProgress] = useState<Progress | null>(null)
  const [achievements, setAchievements] = useState<Achievement[]>([])

  useEffect(() => {
    loadData()
  }, [])

  const loadData = async () => {
    try {
      const [att, prog, ach] = await Promise.all([
        api.get<{ attempts: Attempt[] }>('/api/users/me/attempts'),
        api.get<Progress>('/api/users/me/progress'),
        api.get<{ achievements: Achievement[] }>('/api/users/me/achievements'),
      ])
      setAttempts(att.attempts)
      setProgress(prog)
      setAchievements(ach.achievements)
    } catch (err) {
      console.error(err)
    }
  }

  const unlockedCount = achievements.filter((a) => a.unlocked).length

  return (
    <div className="max-w-4xl mx-auto px-4 py-8">
      <div className="flex items-center gap-4 mb-8">
        {user?.avatar_url && (
          <img src={user.avatar_url} alt="" className="w-16 h-16 rounded-full" />
        )}
        <div>
          <h1 className="text-2xl font-bold">{user?.username}</h1>
          <p className="text-ink-muted text-sm">{user?.email}</p>
        </div>
      </div>

      {progress && (
        <div className="grid grid-cols-2 gap-4 mb-8">
          <div className="bg-surface border border-edge rounded-xl p-5">
            <p className="text-3xl font-bold text-success">{progress.solved_count}</p>
            <p className="text-sm text-ink-muted">해결한 문제</p>
          </div>
          <div className="bg-surface border border-edge rounded-xl p-5">
            <p className="text-3xl font-bold text-accent-hover">{progress.total_attempts}</p>
            <p className="text-sm text-ink-muted">총 시도 횟수</p>
          </div>
        </div>
      )}

      {achievements.length > 0 && (
        <>
          <div className="flex items-center gap-2 mb-4">
            <h2 className="text-lg font-semibold">배지</h2>
            <span className="text-sm text-ink-faint">{unlockedCount}/{achievements.length} 획득</span>
          </div>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-8">
            {achievements.map((a) => (
              <div
                key={a.id}
                className={`flex items-start gap-3 p-4 rounded-lg border ${
                  a.unlocked ? 'bg-success-soft border-success/30' : 'bg-surface border-edge opacity-60'
                }`}
              >
                <Award className={`w-5 h-5 mt-0.5 shrink-0 ${a.unlocked ? 'text-success' : 'text-ink-faint'}`} aria-hidden="true" />
                <div>
                  <p className="font-medium">{a.title}</p>
                  <p className="text-xs text-ink-muted">{a.description}</p>
                </div>
              </div>
            ))}
          </div>
        </>
      )}

      <h2 className="text-lg font-semibold mb-4">시도 기록</h2>
      {attempts.length === 0 ? (
        <p className="text-ink-faint">아직 시도 기록이 없습니다.</p>
      ) : (
        <div className="space-y-2">
          {attempts.map((a) => {
            const config = statusConfig[a.status] || statusConfig.in_progress
            const StatusIcon = config.icon
            return (
              <div key={a.id} className="flex items-center justify-between bg-surface border border-edge rounded-lg px-4 py-3">
                <div className="flex items-center gap-2">
                  <StatusIcon className={`w-4 h-4 ${config.color}`} aria-hidden="true" />
                  <span className="font-medium">{a.problem_id}</span>
                  <span className={`text-sm ${config.color}`}>{a.status}</span>
                </div>
                <div className="text-sm text-ink-faint">
                  {a.duration_seconds ? `${Math.floor(a.duration_seconds / 60)}m ${a.duration_seconds % 60}s` : '-'}
                  <span className="ml-3">{new Date(a.started_at).toLocaleDateString()}</span>
                </div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
