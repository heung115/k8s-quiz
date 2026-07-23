import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { LeaderboardEntry } from '../types'
import { useAuthStore } from '../stores/auth'
import { SectionLabel } from '../components/ui'
import { Trophy } from 'lucide-react'

export function Leaderboard() {
  const { user } = useAuthStore()
  const [entries, setEntries] = useState<LeaderboardEntry[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    api.get<{ leaderboard: LeaderboardEntry[] }>('/api/leaderboard')
      .then((d) => setEntries(d.leaderboard))
      .catch(console.error)
      .finally(() => setLoading(false))
  }, [])

  return (
    <div className="max-w-3xl mx-auto px-5 py-10">
      <SectionLabel>RANKING</SectionLabel>
      <div className="mt-3 flex items-center gap-3">
        <Trophy className="w-6 h-6 text-warning" aria-hidden="true" />
        <h1 className="font-display font-bold text-3xl tracking-tight">리더보드</h1>
      </div>
      <p className="mt-2 text-sm text-ink-muted">해결한 시나리오 수 기준, 동률이면 총 시도 횟수가 적은 순.</p>

      {loading ? (
        <div className="mt-10 space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-16 bg-surface border border-edge-soft animate-pulse" />
          ))}
        </div>
      ) : entries.length === 0 ? (
        <div className="mt-10 border border-edge-soft bg-surface py-16 text-center">
          <p className="font-mono text-sm text-ink-faint">$ kubectl get operators</p>
          <p className="mt-2 text-ink-muted">아직 순위 데이터가 없습니다. 첫 시나리오를 클리어해보세요.</p>
        </div>
      ) : (
        <div className="mt-8 border border-edge bg-surface divide-y divide-edge-soft">
          {/* header row */}
          <div className="grid grid-cols-[3rem_1fr_5rem_5rem] sm:grid-cols-[3rem_1fr_6rem_6rem_6rem] items-center px-4 py-2.5 micro text-ink-faint">
            <span>RANK</span>
            <span>OPERATOR</span>
            <span className="text-right">SOLVED</span>
            <span className="text-right">TRIES</span>
            <span className="hidden sm:block text-right">RATE</span>
          </div>
          {entries.map((e) => {
            const me = e.user_id === user?.id
            const top3 = e.rank <= 3
            return (
              <div
                key={e.user_id}
                className={`grid grid-cols-[3rem_1fr_5rem_5rem] sm:grid-cols-[3rem_1fr_6rem_6rem_6rem] items-center px-4 py-3.5 transition-colors ${
                  me ? 'bg-accent-dim' : 'hover:bg-surface-2'
                }`}
              >
                <span className={`font-display font-bold text-xl ${
                  e.rank === 1 ? 'text-warning' : e.rank === 2 ? 'text-ink' : e.rank === 3 ? 'text-accent-hover' : 'text-ink-faint'
                }`}>
                  {top3 ? `0${e.rank}` : e.rank}
                </span>
                <span className="flex items-center gap-3 min-w-0">
                  {e.avatar_url ? (
                    <img src={e.avatar_url} alt="" className="w-8 h-8 rounded-full border border-edge shrink-0" />
                  ) : (
                    <span className="w-8 h-8 rounded-full border border-edge bg-surface-2 flex items-center justify-center micro text-ink-faint shrink-0">
                      {e.username[0]?.toUpperCase()}
                    </span>
                  )}
                  <span className={`font-medium truncate ${me ? 'text-accent-hover' : ''}`}>
                    {e.username}
                    {me && <span className="micro ml-2 text-accent-hover">YOU</span>}
                  </span>
                </span>
                <span className="text-right font-display font-bold text-success text-lg">{e.solved_count}</span>
                <span className="text-right font-mono text-sm text-ink-muted">{e.total_attempts}</span>
                <span className="hidden sm:block text-right font-mono text-sm text-ink-faint">
                  {e.total_attempts > 0 ? Math.round((e.solved_count / e.total_attempts) * 100) + '%' : '—'}
                </span>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
