import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { Problem, User, Attempt } from '../types'
import { SectionLabel, SevTag, TypeTag } from '../components/ui'
import { RefreshCw, Trash2, ShieldCheck } from 'lucide-react'

type Tab = 'problems' | 'users' | 'attempts'

const TABS: { key: Tab; label: string }[] = [
  { key: 'problems', label: 'PROBLEMS' },
  { key: 'users', label: 'USERS' },
  { key: 'attempts', label: 'ATTEMPTS' },
]

const statusCls: Record<string, string> = {
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
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (tab === 'problems') {
      api.get<{ problems: Problem[] }>('/api/admin/problems').then((d) => setProblems(d.problems)).catch(console.error)
    } else if (tab === 'users') {
      api.get<{ users: User[] }>('/api/admin/users').then((d) => setUsers(d.users)).catch(console.error)
    } else {
      api.get<{ attempts: Attempt[] }>('/api/admin/attempts').then((d) => setAttempts(d.attempts)).catch(console.error)
    }
  }, [tab])

  const handleSync = async () => {
    setBusy(true)
    try {
      const data = await api.post<{ synced: number }>('/api/admin/problems/sync')
      setSyncMsg(`${data.synced}건 동기화 완료`)
      const d = await api.get<{ problems: Problem[] }>('/api/admin/problems')
      setProblems(d.problems)
      setTimeout(() => setSyncMsg(''), 3000)
    } catch (err) {
      setSyncMsg('동기화 실패: ' + (err instanceof Error ? err.message : String(err)))
    } finally {
      setBusy(false)
    }
  }

  const handleDeleteProblem = async (id: string) => {
    if (!confirm(`"${id}" 문제를 삭제할까요?`)) return
    try {
      await api.delete(`/api/admin/problems/${id}`)
      setProblems((prev) => prev.filter((p) => p.id !== id))
    } catch (err) {
      console.error(err)
    }
  }

  const handleChangeRole = async (userId: string, role: string) => {
    try {
      await api.put(`/api/admin/users/${userId}/role`, { role })
      setUsers((prev) => prev.map((u) => (u.id === userId ? { ...u, role: role as User['role'] } : u)))
    } catch (err) {
      console.error(err)
    }
  }

  return (
    <div className="max-w-6xl mx-auto px-5 py-10">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <SectionLabel>CONTROL PLANE</SectionLabel>
          <h1 className="mt-3 font-display font-bold text-3xl tracking-tight flex items-center gap-3">
            <ShieldCheck className="w-7 h-7 text-accent-hover" aria-hidden="true" /> 관리
          </h1>
        </div>
        {tab === 'problems' && (
          <button onClick={handleSync} disabled={busy} className="btn-ghost text-sm py-2">
            <RefreshCw className={`w-4 h-4 ${busy ? 'animate-spin' : ''}`} aria-hidden="true" />
            {busy ? '동기화 중…' : 'Git 저장소 동기화'}
          </button>
        )}
      </div>

      {syncMsg && <p className="mt-4 micro text-success">{syncMsg}</p>}

      {/* tabs */}
      <div className="mt-8 flex gap-1 border-b border-edge" role="tablist" aria-label="관리 섹션">
        {TABS.map((t) => (
          <button
            key={t.key}
            role="tab"
            aria-selected={tab === t.key}
            onClick={() => setTab(t.key)}
            className={`px-4 py-2.5 micro border-b-2 -mb-px transition-colors ${
              tab === t.key ? 'border-accent text-accent-hover' : 'border-transparent text-ink-faint hover:text-ink-muted'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* problems */}
      {tab === 'problems' && (
        <div className="mt-6 border border-edge bg-surface divide-y divide-edge-soft">
          {problems.length === 0 && (
            <p className="p-8 text-center text-ink-faint text-sm">문제가 없습니다. "Git 저장소 동기화"를 실행하세요.</p>
          )}
          {problems.map((p) => (
            <div key={p.id} className="flex items-center gap-4 px-4 py-3.5 hover:bg-surface-2 transition-colors">
              <div className="min-w-0 flex-1">
                <p className="font-medium truncate">{p.title}</p>
                <p className="font-mono text-xs text-ink-faint mt-0.5">{p.id} · {p.verify_type} · {p.timeout_minutes}min</p>
              </div>
              <div className="flex items-center gap-2 shrink-0">
                <SevTag difficulty={p.difficulty} />
                <TypeTag type={p.type} />
                <span className="micro text-ink-faint hidden sm:inline">{p.category}</span>
              </div>
              <button
                onClick={() => handleDeleteProblem(p.id)}
                className="p-2 text-ink-faint hover:text-danger transition-colors shrink-0"
                aria-label={`${p.id} 삭제`}
              >
                <Trash2 className="w-4 h-4" aria-hidden="true" />
              </button>
            </div>
          ))}
        </div>
      )}

      {/* users */}
      {tab === 'users' && (
        <div className="mt-6 border border-edge bg-surface divide-y divide-edge-soft">
          {users.map((u) => (
            <div key={u.id} className="flex items-center gap-4 px-4 py-3.5">
              {u.avatar_url ? (
                <img src={u.avatar_url} alt="" className="w-8 h-8 rounded-full border border-edge" />
              ) : (
                <span className="w-8 h-8 rounded-full border border-edge bg-surface-2 flex items-center justify-center micro text-ink-faint">
                  {u.username[0]?.toUpperCase()}
                </span>
              )}
              <div className="min-w-0 flex-1">
                <p className="font-medium truncate">{u.username}</p>
                <p className="font-mono text-xs text-ink-faint truncate">{u.email || u.id.slice(0, 8)}</p>
              </div>
              <select
                value={u.role}
                onChange={(e) => handleChangeRole(u.id, e.target.value)}
                className="bg-surface-2 border border-edge px-2 py-1.5 font-mono text-xs focus:border-accent/60 focus:outline-none"
                aria-label={`${u.username} 권한`}
              >
                <option value="user">user</option>
                <option value="admin">admin</option>
              </select>
            </div>
          ))}
        </div>
      )}

      {/* attempts */}
      {tab === 'attempts' && (
        <div className="mt-6 border border-edge bg-surface divide-y divide-edge-soft">
          {attempts.length === 0 && <p className="p-8 text-center text-ink-faint text-sm">아직 시도 기록이 없습니다.</p>}
          {attempts.map((a) => (
            <div key={a.id} className="flex items-center gap-4 px-4 py-3.5">
              <span className="font-mono text-sm font-medium">{a.problem_id}</span>
              <span className={`micro ${statusCls[a.status] || 'text-ink-muted'}`}>{a.status.toUpperCase()}</span>
              <span className="ml-auto font-mono text-xs text-ink-faint">
                user {a.user_id.slice(0, 8)}
                {a.duration_seconds != null && ` · ${a.duration_seconds}s`}
                <span className="hidden sm:inline"> · {new Date(a.started_at).toLocaleString()}</span>
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
