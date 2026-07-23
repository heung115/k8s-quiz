import { useEffect, useState, useCallback } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session } from '../types'
import { useSessionStore } from '../stores/session'
import { Terminal } from '../components/Terminal'
import { Wrench, Search, Lightbulb, RotateCcw, CheckCircle2, XCircle, Play, Square, AlertTriangle, Rocket } from 'lucide-react'

function useCountdown(timeoutAt: string | undefined, active: boolean) {
  const [remaining, setRemaining] = useState<number | null>(null)

  useEffect(() => {
    if (!timeoutAt || !active) {
      setRemaining(null)
      return
    }
    const deadline = new Date(timeoutAt).getTime()
    const tick = () => {
      const diff = Math.max(0, Math.floor((deadline - Date.now()) / 1000))
      setRemaining(diff)
    }
    tick()
    const interval = setInterval(tick, 1000)
    return () => clearInterval(interval)
  }, [timeoutAt, active])

  return remaining
}

function formatTime(totalSeconds: number): string {
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${minutes}:${seconds.toString().padStart(2, '0')}`
}

export function ProblemPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [problem, setProblem] = useState<Problem | null>(null)
  const [starting, setStarting] = useState(false)
  const [verifying, setVerifying] = useState(false)
  const [error, setError] = useState('')
  const [showHint, setShowHint] = useState(false)
  const [selectedChoice, setSelectedChoice] = useState('')
  const { session, stage, stageMessage, crashed, verifyResult, setSession, setCrashed, setVerifyResult, clear } = useSessionStore()

  const remaining = useCountdown(session?.timeout_at, !!session)
  const isUrgent = remaining !== null && remaining <= 300

  const restoreSession = useCallback(async () => {
    try {
      const data = await api.get<Session>('/api/sessions/current')
      if (data && data.problem_id === id) {
        setSession(data)
      }
    } catch {
      // no active session for this problem
    }
  }, [id, setSession])

  useEffect(() => {
    loadProblem()
    restoreSession()
    return () => clear()
  }, [id])

  useEffect(() => {
    if (remaining === 0 && session) {
      clear()
      navigate('/')
    }
  }, [remaining, session])

  const loadProblem = async () => {
    try {
      const data = await api.get<Problem>(`/api/problems/${id}`)
      setProblem(data)
    } catch {
      setError('문제를 찾을 수 없습니다.')
    }
  }

  const handleStart = async () => {
    setStarting(true)
    setError('')
    try {
      const data = await api.post<Session>(`/api/problems/${id}/start`)
      setSession(data)
    } catch (err: any) {
      setError(err.message)
    } finally {
      setStarting(false)
    }
  }

  const handleReset = async () => {
    try {
      await api.post(`/api/problems/${id}/reset`)
      setVerifyResult(null)
      setCrashed(false)
    } catch (err: any) {
      setError(err.message)
    }
  }

  const handleVerify = async () => {
    setVerifying(true)
    setVerifyResult(null)
    try {
      const data = await api.post<{ success: boolean; log: string }>(`/api/problems/${id}/verify`)
      setVerifyResult(data)
    } catch (err: any) {
      setError(err.message)
    } finally {
      setVerifying(false)
    }
  }

  const handleSubmitChoice = async () => {
    if (!selectedChoice) return
    setVerifying(true)
    try {
      const data = await api.post<{ success: boolean }>(`/api/problems/${id}/submit`, { choice_id: selectedChoice })
      setVerifyResult({ success: data.success, log: '' })
    } catch (err: any) {
      setError(err.message)
    } finally {
      setVerifying(false)
    }
  }

  const handleEnd = async () => {
    try {
      await api.delete('/api/sessions/current')
    } catch {}
    clear()
    navigate('/')
  }

  if (!problem) {
    return <div className="max-w-7xl mx-auto px-4 py-8 text-ink-faint">Loading...</div>
  }

  return (
    <div className="h-[calc(100vh-3.5rem)] flex">
      <div className="w-96 border-r border-edge overflow-y-auto p-5 flex flex-col">
        <div className="mb-4">
          <div className="flex items-center gap-2 mb-2">
            <span className="text-xs px-2 py-0.5 rounded bg-surface-2 text-ink-muted font-mono">{problem.category}</span>
            <span className="text-xs px-2 py-0.5 rounded bg-surface-2 text-ink-muted font-mono">{problem.difficulty}</span>
            <span className="flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-surface-2 text-ink-muted">
              {problem.type === 'fix' ? (
                <><Wrench className="w-3 h-3" aria-hidden="true" /> Fix</>
              ) : problem.type === 'find' ? (
                <><Search className="w-3 h-3" aria-hidden="true" /> Find</>
              ) : (
                <><Rocket className="w-3 h-3" aria-hidden="true" /> Deploy</>
              )}
            </span>
          </div>
          <h1 className="text-xl font-bold">{problem.title}</h1>
        </div>

        <div className="text-sm text-ink whitespace-pre-wrap mb-4 flex-1">
          {problem.description}
        </div>

        {problem.hint && (
          <div className="mb-4">
            <button
              onClick={() => setShowHint(!showHint)}
              className="flex items-center gap-1 text-xs text-warning hover:opacity-80"
            >
              <Lightbulb className="w-3.5 h-3.5" aria-hidden="true" />
              {showHint ? '힌트 숨기기' : '힌트 보기'}
            </button>
            {showHint && (
              <p className="mt-2 text-xs text-ink-muted bg-surface-2 p-3 rounded-lg whitespace-pre-wrap">
                {problem.hint}
              </p>
            )}
          </div>
        )}

        {problem.type === 'find' && problem.verify_type === 'choice' && problem.choices && session && (
          <div className="mb-4 space-y-2">
            <p className="text-sm font-medium text-ink">원인을 선택하세요:</p>
            {problem.choices.map((choice) => (
              <label key={choice.id} className="flex items-center gap-2 text-sm cursor-pointer">
                <input
                  type="radio"
                  name="choice"
                  value={choice.id}
                  checked={selectedChoice === choice.id}
                  onChange={(e) => setSelectedChoice(e.target.value)}
                  className="accent-accent"
                />
                {choice.text}
              </label>
            ))}
            <button
              onClick={handleSubmitChoice}
              disabled={!selectedChoice || verifying}
              className="w-full mt-2 px-4 py-2 bg-accent hover:bg-accent-hover disabled:bg-surface-3 disabled:text-ink-faint rounded-lg text-sm font-medium transition-colors"
            >
              제출
            </button>
          </div>
        )}

        <div className="space-y-3 border-t border-edge pt-4">
          {session && remaining !== null && (
            <div className={`flex items-center justify-between text-sm font-mono px-3 py-2 rounded-lg border ${
              isUrgent
                ? 'bg-danger-soft text-danger border-danger/30 animate-pulse'
                : 'bg-surface-2 text-ink border-edge'
            }`}>
              <span>남은 시간</span>
              <span className="text-base font-semibold">{formatTime(remaining)}</span>
            </div>
          )}

          {crashed && (
            <div className="p-3 rounded-lg text-sm bg-danger-soft text-danger border border-danger/30">
              <span className="flex items-center gap-1.5 font-medium">
                <AlertTriangle className="w-4 h-4" aria-hidden="true" />
                컨테이너가 중단되었습니다.
              </span>
              <p className="mt-1 text-xs text-danger/80">리셋 버튼으로 환경을 다시 시작하세요.</p>
            </div>
          )}

          {!crashed && stage && session && (
            <div className="text-xs text-accent-hover">
              [{stage}] {stageMessage}
            </div>
          )}

          {verifyResult && (
            <div className={`p-3 rounded-lg text-sm border ${verifyResult.success ? 'bg-success-soft text-success border-success/30' : 'bg-danger-soft text-danger border-danger/30'}`}>
              <span className="flex items-center gap-1.5 font-medium">
                {verifyResult.success ? (
                  <><CheckCircle2 className="w-4 h-4" aria-hidden="true" /> 문제 해결 성공!</>
                ) : (
                  <><XCircle className="w-4 h-4" aria-hidden="true" /> 아직 해결되지 않았습니다.</>
                )}
              </span>
              {verifyResult.log && (
                <pre className="mt-2 text-xs text-ink-muted overflow-x-auto whitespace-pre-wrap">{verifyResult.log}</pre>
              )}
            </div>
          )}

          {error && (
            <div className="p-3 rounded-lg text-sm bg-danger-soft text-danger border border-danger/30">{error}</div>
          )}

          {!session ? (
            <button
              onClick={handleStart}
              disabled={starting}
              className="w-full flex items-center justify-center gap-2 px-4 py-2.5 bg-accent hover:bg-accent-hover disabled:bg-surface-3 rounded-lg font-medium transition-colors"
            >
              <Play className="w-4 h-4" aria-hidden="true" />
              {starting ? '환경 준비 중...' : '문제 시작'}
            </button>
          ) : (
            <div className="flex gap-2">
              {(problem.verify_type === 'script' || problem.verify_type === 'text') && (
                <button
                  onClick={handleVerify}
                  disabled={verifying || crashed}
                  className="flex-1 flex items-center justify-center gap-1.5 px-4 py-2 bg-success-soft text-success border border-success/30 hover:bg-success/20 disabled:opacity-50 rounded-lg text-sm font-medium transition-colors"
                >
                  <CheckCircle2 className="w-4 h-4" aria-hidden="true" />
                  {verifying ? '검증 중...' : '확인'}
                </button>
              )}
              <button
                onClick={handleReset}
                className="flex items-center justify-center gap-1.5 px-4 py-2 bg-surface-2 hover:bg-surface-3 rounded-lg text-sm transition-colors"
              >
                <RotateCcw className="w-3.5 h-3.5" aria-hidden="true" />
                리셋
              </button>
              <button
                onClick={handleEnd}
                className="flex items-center justify-center gap-1.5 px-4 py-2 bg-surface-2 hover:bg-danger-soft hover:text-danger rounded-lg text-sm transition-colors"
              >
                <Square className="w-3.5 h-3.5" aria-hidden="true" />
                종료
              </button>
            </div>
          )}
        </div>
      </div>

      <div className="flex-1 bg-surface">
        {session ? (
          <Terminal />
        ) : (
          <div className="h-full flex items-center justify-center text-ink-faint">
            <div className="text-center">
              <p className="text-4xl mb-4 font-mono text-ink-faint">k8s</p>
              <p>"문제 시작"을 클릭하면 터미널이 열립니다.</p>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
