import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { Problem, User, Attempt } from '../types'
import { RefreshCw, Trash2 } from 'lucide-react'

type Tab = 'problems' | 'users' | 'attempts'

const attemptStatusColor: Record<string, string> = {
  success: 'text-success',
  failed: 'text-danger',
  in_progress: 'text-warning',
  timeout: 'text-warning',
}

export function Admin() {
  const [tab, setTab] = useState<Tab>('problems')
  const [problems, setProblems] = useState<Problem[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [attempts, setAttempts] = useState<Attempt[]>([])
  const [syncMsg, setSyncMsg] = useState('')

  useEffect(() => {
    loadData()
  }, [tab])

  const loadData = async () => {
    try {
      if (tab === 'problems') {
        const data = await api.get<{ problems: Problem[] }>('/api/admin/problems')
        setProblems(data.problems)
      } else if (tab === 'users') {
        const data = await api.get<{ users: User[] }>('/api/admin/users')
        setUsers(data.users)
      } else {
        const data = await api.get<{ attempts: Attempt[] }>('/api/admin/attempts')
        setAttempts(data.attempts)
      }
    } catch (err) {
      console.error(err)
    }
  }

  const handleSync = async () => {
    try {
      const data = await api.post<{ synced: number }>('/api/admin/problems/sync')
      setSyncMsg(`${data.synced} problems synced`)
      loadData()
      setTimeout(() => setSyncMsg(''), 3000)
    } catch (err: any) {
      setSyncMsg('Sync failed: ' + err.message)
    }
  }

  const handleDeleteProblem = async (id: string) => {
    if (!confirm(`Delete problem "${id}"?`)) return
    try {
      await api.delete(`/api/admin/problems/${id}`)
      loadData()
    } catch (err) {
      console.error(err)
    }
  }

  const handleChangeRole = async (userId: string, role: string) => {
    try {
      await api.put(`/api/admin/users/${userId}/role`, { role })
      loadData()
    } catch (err) {
      console.error(err)
    }
  }

  return (
    <div className="max-w-6xl mx-auto px-4 py-8">
      <h1 className="text-2xl font-bold mb-6">Admin</h1>

      <div className="flex gap-2 mb-6">
        {(['problems', 'users', 'attempts'] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-4 py-2 rounded-lg text-sm font-medium transition-colors ${
              tab === t ? 'bg-accent text-ink' : 'bg-surface-2 text-ink-muted hover:text-ink'
            }`}
          >
            {t}
          </button>
        ))}
        {tab === 'problems' && (
          <button
            onClick={handleSync}
            className="ml-auto flex items-center gap-1.5 px-4 py-2 bg-success-soft text-success border border-success/30 hover:bg-success/20 rounded-lg text-sm font-medium transition-colors"
          >
            <RefreshCw className="w-3.5 h-3.5" aria-hidden="true" />
            Sync Problems
          </button>
        )}
      </div>

      {syncMsg && <p className="mb-4 text-sm text-success">{syncMsg}</p>}

      {tab === 'problems' && (
        <div className="space-y-2">
          {problems.map((p) => (
            <div key={p.id} className="flex items-center justify-between bg-surface border border-edge rounded-lg px-4 py-3">
              <div>
                <span className="font-medium">{p.title}</span>
                <span className="ml-3 text-xs text-ink-faint font-mono">{p.id}</span>
              </div>
              <div className="flex items-center gap-3">
                <span className="text-xs text-ink-faint">{p.category} / {p.difficulty}</span>
                <button
                  onClick={() => handleDeleteProblem(p.id)}
                  className="flex items-center gap-1 text-xs text-danger hover:opacity-80"
                >
                  <Trash2 className="w-3 h-3" aria-hidden="true" />
                  Delete
                </button>
              </div>
            </div>
          ))}
          {problems.length === 0 && <p className="text-ink-faint">No problems. Click "Sync Problems" to load from repo.</p>}
        </div>
      )}

      {tab === 'users' && (
        <div className="space-y-2">
          {users.map((u) => (
            <div key={u.id} className="flex items-center justify-between bg-surface border border-edge rounded-lg px-4 py-3">
              <div className="flex items-center gap-3">
                {u.avatar_url && <img src={u.avatar_url} alt="" className="w-8 h-8 rounded-full" />}
                <div>
                  <span className="font-medium">{u.username}</span>
                  <span className="ml-2 text-xs text-ink-faint">{u.email}</span>
                </div>
              </div>
              <select
                value={u.role}
                onChange={(e) => handleChangeRole(u.id, e.target.value)}
                className="bg-surface-2 border border-edge rounded px-2 py-1 text-xs"
                aria-label={`Role for ${u.username}`}
              >
                <option value="user">user</option>
                <option value="admin">admin</option>
              </select>
            </div>
          ))}
        </div>
      )}

      {tab === 'attempts' && (
        <div className="space-y-2">
          {attempts.map((a) => (
            <div key={a.id} className="flex items-center justify-between bg-surface border border-edge rounded-lg px-4 py-3">
              <div>
                <span className="font-medium">{a.problem_id}</span>
                <span className="ml-3 text-xs text-ink-faint">user: {a.user_id.slice(0, 8)}</span>
              </div>
              <div className="text-sm text-ink-faint">
                <span className={attemptStatusColor[a.status] || 'text-ink-muted'}>
                  {a.status}
                </span>
                {a.duration_seconds && <span className="ml-2">{a.duration_seconds}s</span>}
              </div>
            </div>
          ))}
          {attempts.length === 0 && <p className="text-ink-faint">No attempts yet.</p>}
        </div>
      )}
    </div>
  )
}
