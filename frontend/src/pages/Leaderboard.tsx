import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { LeaderboardEntry } from '../types'
import { Trophy } from 'lucide-react'

const rankAccent: Record<number, string> = {
  1: 'text-warning',
  2: 'text-ink',
  3: 'text-accent-hover',
}

export function Leaderboard() {
  const [entries, setEntries] = useState<LeaderboardEntry[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    api.get<{ leaderboard: LeaderboardEntry[] }>('/api/leaderboard')
      .then((d) => setEntries(d.leaderboard))
      .catch(console.error)
      .finally(() => setLoading(false))
  }, [])

  return (
    <div className="max-w-3xl mx-auto px-4 py-8">
      <div className="flex items-center gap-2 mb-8">
        <Trophy className="w-6 h-6 text-warning" aria-hidden="true" />
        <h1 className="text-2xl font-bold">리더보드</h1>
      </div>

      {loading ? (
        <div className="text-center py-12 text-ink-faint">Loading...</div>
      ) : entries.length === 0 ? (
        <div className="text-center py-12 text-ink-faint">아직 순위 데이터가 없습니다. 첫 문제를 해결해보세요!</div>
      ) : (
        <div className="space-y-2">
          {entries.map((e) => (
            <div
              key={e.user_id}
              className={`flex items-center gap-4 bg-surface border border-edge rounded-lg px-4 py-3 ${
                e.rank <= 3 ? 'border-l-2 border-l-warning' : ''
              }`}
            >
              <span className={`w-8 text-center font-mono font-bold text-lg ${rankAccent[e.rank] || 'text-ink-faint'}`}>
                {e.rank}
              </span>
              {e.avatar_url ? (
                <img src={e.avatar_url} alt="" className="w-9 h-9 rounded-full" />
              ) : (
                <div className="w-9 h-9 rounded-full bg-surface-3" />
              )}
              <span className="font-medium flex-1">{e.username}</span>
              <div className="text-right">
                <p className="font-bold text-success">{e.solved_count} <span className="text-xs font-normal text-ink-muted">해결</span></p>
                <p className="text-xs text-ink-faint">{e.total_attempts}회 시도</p>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
