import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session } from '../types'
import { Wrench, Search, Clock, PlayCircle, Rocket } from 'lucide-react'

const categories = ['', 'pod', 'network', 'storage', 'rbac', 'scheduling', 'config']
const difficulties = ['', 'easy', 'medium', 'hard']

const difficultyOrder = ['easy', 'medium', 'hard'] as const

const difficultyMeta: Record<string, { label: string; badge: string; bar: string; hint: string }> = {
  easy: {
    label: 'Easy',
    badge: 'bg-success-soft text-success border-success/30',
    bar: 'border-l-success',
    hint: '기본 개념으로 풀 수 있는 문제',
  },
  medium: {
    label: 'Medium',
    badge: 'bg-warning-soft text-warning border-warning/30',
    bar: 'border-l-warning',
    hint: '여러 리소스를 함께 봐야 하는 문제',
  },
  hard: {
    label: 'Hard',
    badge: 'bg-danger-soft text-danger border-danger/30',
    bar: 'border-l-danger',
    hint: '심화 진단이 필요한 문제',
  },
}

function ProblemCard({ p }: { p: Problem }) {
  const meta = difficultyMeta[p.difficulty]
  return (
    <Link
      to={`/problems/${p.id}`}
      className={`block p-5 bg-surface border border-edge border-l-2 ${meta.bar} rounded-r-xl hover:border-accent hover:border-l-accent transition-colors group`}
    >
      <div className="flex items-center justify-between mb-3">
        <span className="bg-surface-2 px-2 py-0.5 rounded font-mono text-xs text-ink-muted">{p.category}</span>
        <span className="flex items-center gap-1 text-xs text-ink-faint">
          {p.type === 'fix' ? (
            <><Wrench className="w-3 h-3" aria-hidden="true" /> Fix</>
          ) : p.type === 'find' ? (
            <><Search className="w-3 h-3" aria-hidden="true" /> Find</>
          ) : (
            <><Rocket className="w-3 h-3" aria-hidden="true" /> Deploy</>
          )}
        </span>
      </div>
      <h3 className="font-semibold mb-1 group-hover:text-accent-hover transition-colors">{p.title}</h3>
      <p className="text-sm text-ink-muted line-clamp-2 mb-3">{p.description}</p>
      <div className="flex items-center gap-1 text-xs text-ink-faint">
        <Clock className="w-3 h-3" aria-hidden="true" />
        {p.timeout_minutes}min
      </div>
    </Link>
  )
}

export function Dashboard() {
  const [problems, setProblems] = useState<Problem[]>([])
  const [category, setCategory] = useState('')
  const [difficulty, setDifficulty] = useState('')
  const [loading, setLoading] = useState(true)
  const [activeSession, setActiveSession] = useState<Session | null>(null)

  useEffect(() => {
    loadProblems()
  }, [category, difficulty])

  useEffect(() => {
    api.get<Session>('/api/sessions/current')
      .then(setActiveSession)
      .catch(() => setActiveSession(null))
  }, [])

  const loadProblems = async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams()
      if (category) params.set('category', category)
      if (difficulty) params.set('difficulty', difficulty)
      const data = await api.get<{ problems: Problem[] }>(`/api/problems?${params}`)
      setProblems(data.problems)
    } catch (err) {
      console.error(err)
    } finally {
      setLoading(false)
    }
  }

  const groups = difficultyOrder
    .map((level) => ({ level, items: problems.filter((p) => p.difficulty === level) }))
    .filter((g) => g.items.length > 0)

  return (
    <div className="max-w-7xl mx-auto px-4 py-8">
      <div className="mb-8">
        <h1 className="text-2xl font-bold mb-2">문제 목록</h1>
        <p className="text-ink-muted text-sm">Kubernetes 트러블슈팅 문제를 선택하고 해결해보세요.</p>
      </div>

      {activeSession && (
        <Link
          to={`/problems/${activeSession.problem_id}`}
          className="flex items-center gap-3 mb-6 p-4 bg-accent-soft border border-accent/30 rounded-xl hover:border-accent transition-colors"
        >
          <PlayCircle className="w-5 h-5 text-accent-hover" aria-hidden="true" />
          <div className="flex-1">
            <p className="font-medium text-ink">진행 중인 세션이 있습니다</p>
            <p className="text-sm text-ink-muted font-mono">{activeSession.problem_id}</p>
          </div>
          <span className="text-sm text-accent-hover">계속하기 →</span>
        </Link>
      )}

      <div className="flex gap-3 mb-8">
        <select
          value={category}
          onChange={(e) => setCategory(e.target.value)}
          className="bg-surface-2 border border-edge rounded-lg px-3 py-2 text-sm"
          aria-label="Filter by category"
        >
          {categories.map((c) => (
            <option key={c} value={c}>{c || 'All Categories'}</option>
          ))}
        </select>
        <select
          value={difficulty}
          onChange={(e) => setDifficulty(e.target.value)}
          className="bg-surface-2 border border-edge rounded-lg px-3 py-2 text-sm"
          aria-label="Filter by difficulty"
        >
          {difficulties.map((d) => (
            <option key={d} value={d}>{d || 'All Difficulties'}</option>
          ))}
        </select>
      </div>

      {loading ? (
        <div className="text-center py-12 text-ink-faint">Loading...</div>
      ) : problems.length === 0 ? (
        <div className="text-center py-12 text-ink-faint">문제가 없습니다.</div>
      ) : (
        <div className="space-y-10">
          {groups.map((g) => {
            const meta = difficultyMeta[g.level]
            return (
              <section key={g.level}>
                <div className="flex items-center gap-3 mb-4">
                  <span className={`text-xs px-2 py-0.5 rounded border font-mono ${meta.badge}`}>
                    {meta.label}
                  </span>
                  <span className="text-sm text-ink-muted">{meta.hint}</span>
                  <span className="text-xs text-ink-faint ml-auto">{g.items.length} 문제</span>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
                  {g.items.map((p) => (
                    <ProblemCard key={p.id} p={p} />
                  ))}
                </div>
              </section>
            )
          })}
        </div>
      )}
    </div>
  )
}
