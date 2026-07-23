import { useEffect, useState, useCallback } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { api } from '../api/client'
import { Problem, Session } from '../types'
import { useSessionStore } from '../stores/session'
import { Terminal } from '../components/Terminal'
import { SevTag, TypeTag, categoryMeta } from '../components/ui'
import {
  Lightbulb, RotateCcw, CheckCircle2, XCircle, Play, Square,
  AlertTriangle, ChevronLeft, Timer, Wifi, WifiOff,
} from 'lucide-react'

function useCountdown(timeoutAt: string | undefined, active: boolean) {
  const [remaining, setRemaining] = useState<number | null>(null)
  useEffect(() => {
    if (!timeoutAt || !active) { setRemaining(null); return }
    const deadline = new Date(timeoutAt).getTime()
    const tick = () => setRemaining(Math.max(0, Math.floor((deadline - Date.now()) / 1000)))
    tick()
    const interval = setInterval(tick, 1000)
    return () => clearInterval(interval)
  }, [timeoutAt, active])
  return remaining
}

function formatTime(s: number): string {
  return `${Math.floor(s / 60)}:${(s % 60).toString().padStart(2, '0')}`
}

/* boot → setup → ready timeline */
function StageTimeline({ status, crashed }: { status: string; crashed: boolean }) {
  const steps = [
    { key: 'booting',    label: '클러스터 부팅', detail: 'k3s 기동 대기' },
    { key: 'setting_up', label: '시나리오 주입', detail: 'setup.sh 실행' },
    { key: 'ready',      label: '대기',          detail: '터미널 접속 가능' },
  ]
  const idx = status === 'ready' ? 3 : status === 'setting_up' ? 1 : 0

  return (
    <ol className="space-y-0">
      {steps.map((s, i) => {
        const done = i < idx || status === 'ready'
        const active = i === idx && status !== 'ready'
        return (
          <li key={s.key} className="flex gap-3">
            <div className="flex flex-col items-center">
              <span
                className={`w-2.5 h-2.5 mt-1 shrink-0 ${
                  crashed ? 'bg-danger' : done ? 'bg-success' : active ? 'bg-accent-hover animate-pulse-dot' : 'bg-edge'
                }`}
                aria-hidden="true"
              />
              {i < steps.length - 1 && <span className={`w-px flex-1 my-1 ${done ? 'bg-success/50' : 'bg-edge-soft'}`} aria-hidden="true" />}
            </div>
            <div className="pb-4">
              <p className={`text-sm font-medium leading-tight ${done ? 'text-ink' : active ? 'text-accent-hover' : 'text-ink-faint'}`}>
                {s.label}
                {active && <span className="ml-2 micro text-accent-hover">진행 중…</span>}
              </p>
              <p className="micro text-ink-faint mt-0.5">{s.detail}</p>
            </div>
          </li>
        )
      })}
    </ol>
  )
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
  const { session, stage, crashed, verifyResult, wsConnected, setSession, setCrashed, setVerifyResult, clear } = useSessionStore()

  const remaining = useCountdown(session?.timeout_at, !!session)
  const isUrgent = remaining !== null && remaining <= 300

  const restoreSession = useCallback(async () => {
    try {
      const data = await api.get<Session>('/api/sessions/current')
      if (data && data.problem_id === id) setSession(data)
    } catch { /* no active session */ }
  }, [id, setSession])

  useEffect(() => {
    api.get<Problem>(`/api/problems/${id}`).then(setProblem).catch(() => setError('문제를 찾을 수 없습니다.'))
    restoreSession()
    return () => clear()
  }, [id])

  useEffect(() => {
    if (remaining === 0 && session) { clear(); navigate('/') }
  }, [remaining, session])

  const handleStart = async () => {
    setStarting(true); setError('')
    try { setSession(await api.post<Session>(`/api/problems/${id}/start`)) }
    catch (err: any) { setError(err.message) }
    finally { setStarting(false) }
  }

  const handleReset = async () => {
    try { await api.post(`/api/problems/${id}/reset`); setVerifyResult(null); setCrashed(false) }
    catch (err: any) { setError(err.message) }
  }

  const handleVerify = async () => {
    setVerifying(true); setVerifyResult(null)
    try { setVerifyResult(await api.post<{ success: boolean; log: string }>(`/api/problems/${id}/verify`)) }
    catch (err: any) { setError(err.message) }
    finally { setVerifying(false) }
  }

  const handleSubmitChoice = async () => {
    if (!selectedChoice) return
    setVerifying(true)
    try {
      const data = await api.post<{ success: boolean }>(`/api/problems/${id}/submit`, { choice_id: selectedChoice })
      setVerifyResult({ success: data.success, log: '' })
    } catch (err: any) { setError(err.message) }
    finally { setVerifying(false) }
  }

  const handleEnd = async () => {
    try { await api.delete('/api/sessions/current') } catch {}
    clear(); navigate('/')
  }

  if (!problem) {
    return (
      <div className="max-w-7xl mx-auto px-5 py-10 micro text-ink-faint">
        LOADING SCENARIO<span className="animate-blink">▍</span>
      </div>
    )
  }

  const cat = categoryMeta[problem.category]
  const CatIcon = cat?.icon
  const isChoice = problem.verify_type === 'choice'
  const status = session?.status || 'idle'

  return (
    <div className="h-[calc(100vh-3.5rem)] flex flex-col">
      {/* ---- incident header bar ---- */}
      <div className="border-b border-edge bg-surface px-5 py-3 flex items-center gap-4 flex-wrap">
        <Link to="/" className="text-ink-faint hover:text-ink transition-colors p-1 -ml-1" aria-label="콘솔로 돌아가기">
          <ChevronLeft className="w-5 h-5" aria-hidden="true" />
        </Link>
        <div className="flex items-center gap-3 min-w-0">
          <h1 className="font-display font-semibold text-lg truncate">{problem.title}</h1>
          <div className="hidden sm:flex items-center gap-2">
            <SevTag difficulty={problem.difficulty} />
            <TypeTag type={problem.type} />
            <span className="micro text-ink-faint inline-flex items-center gap-1.5">
              {CatIcon && <CatIcon className="w-3.5 h-3.5" aria-hidden="true" />}
              {cat?.label || problem.category}
            </span>
          </div>
        </div>
        <div className="ml-auto flex items-center gap-4">
          {session && (
            <span className={`hidden sm:inline-flex items-center gap-1.5 micro ${wsConnected ? 'text-success' : 'text-ink-faint'}`}>
              {wsConnected ? <Wifi className="w-3.5 h-3.5" aria-hidden="true" /> : <WifiOff className="w-3.5 h-3.5" aria-hidden="true" />}
              {wsConnected ? 'TTY LINKED' : 'TTY OFFLINE'}
            </span>
          )}
          {session && remaining !== null && (
            <span
              className={`inline-flex items-center gap-2 font-mono text-sm px-3 py-1.5 border ${
                isUrgent ? 'border-danger/50 bg-danger-soft text-danger animate-pulse' : 'border-edge bg-surface-2 text-ink'
              }`}
              role="timer"
              aria-label="남은 시간"
            >
              <Timer className="w-4 h-4" aria-hidden="true" />
              {formatTime(remaining)}
            </span>
          )}
        </div>
      </div>

      <div className="flex-1 flex min-h-0">
        {/* ---- left panel ---- */}
        <aside className="w-[380px] shrink-0 border-r border-edge overflow-y-auto bg-canvas">
          <div className="p-5 space-y-5">
            {/* description */}
            <div>
              <p className="micro text-ink-faint mb-2"><span className="text-accent">//</span> INCIDENT BRIEF</p>
              <p className="text-sm text-ink-muted leading-relaxed whitespace-pre-line">{problem.description}</p>
            </div>

            {/* stages / start */}
            {!session ? (
              <div className="border border-edge bg-surface p-4">
                <p className="text-sm text-ink-muted leading-relaxed">
                  환경을 시작하면 격리된 k3s 클러스터가 부팅되고,
                  터미널에서 <span className="font-mono text-info">kubectl</span>로 문제를 진단할 수 있습니다.
                </p>
                <button onClick={handleStart} disabled={starting} className="btn-primary w-full mt-4 text-sm py-2.5">
                  <Play className="w-4 h-4" aria-hidden="true" />
                  {starting ? '환경 준비 중…' : '환경 시작'}
                </button>
                {starting && <p className="micro text-accent-hover mt-3 text-center">컨테이너 생성 → k3s 부팅 → 시나리오 주입</p>}
              </div>
            ) : (
              <div className="border border-edge bg-surface p-4">
                <p className="micro text-ink-faint mb-3"><span className="text-accent">//</span> ENVIRONMENT</p>
                <StageTimeline status={crashed ? 'failed' : status} crashed={crashed} />
              </div>
            )}

            {/* crash */}
            {crashed && (
              <div className="border border-danger/40 bg-danger-soft p-4">
                <p className="flex items-center gap-2 text-sm font-semibold text-danger">
                  <AlertTriangle className="w-4 h-4" aria-hidden="true" /> 컨테이너가 중단되었습니다
                </p>
                <p className="mt-1 text-xs text-danger/80">리셋으로 환경을 다시 띄우세요. 이전 작업은 사라집니다.</p>
              </div>
            )}

            {/* verify result — terminal style */}
            {verifyResult && (
              <div className={`border ${verifyResult.success ? 'border-success/40' : 'border-danger/40'} bg-terminal`}>
                <div className={`px-3 py-2 border-b flex items-center gap-2 ${verifyResult.success ? 'border-success/30' : 'border-danger/30'}`}>
                  {verifyResult.success
                    ? <CheckCircle2 className="w-4 h-4 text-success" aria-hidden="true" />
                    : <XCircle className="w-4 h-4 text-danger" aria-hidden="true" />}
                  <span className={`micro ${verifyResult.success ? 'text-success' : 'text-danger'}`}>
                    {verifyResult.success ? 'VERIFICATION PASSED' : 'VERIFICATION FAILED'}
                  </span>
                </div>
                <div className="p-3 font-mono text-xs leading-relaxed">
                  {verifyResult.success ? (
                    <p className="text-success">✓ 시나리오 클리어! 프로필에 기록되었습니다.</p>
                  ) : (
                    <p className="text-danger">✗ 아직 조건을 만족하지 않습니다. 터미널에서 상태를 다시 확인하세요.</p>
                  )}
                  {verifyResult.log && (
                    <pre className="mt-2 text-ink-muted whitespace-pre-wrap overflow-x-auto">{verifyResult.log}</pre>
                  )}
                </div>
              </div>
            )}

            {/* choice answers */}
            {isChoice && session && problem.choices && (
              <div className="border border-edge bg-surface p-4">
                <p className="micro text-ink-faint mb-3"><span className="text-accent">//</span> 원인 선택</p>
                <div className="space-y-2" role="radiogroup" aria-label="원인 선택">
                  {problem.choices.map((choice) => (
                    <label
                      key={choice.id}
                      className={`flex items-start gap-3 border p-3 cursor-pointer transition-colors text-sm ${
                        selectedChoice === choice.id
                          ? 'border-accent/60 bg-accent-dim text-ink'
                          : 'border-edge text-ink-muted hover:border-edge hover:bg-surface-2'
                      }`}
                    >
                      <input
                        type="radio"
                        name="choice"
                        value={choice.id}
                        checked={selectedChoice === choice.id}
                        onChange={() => setSelectedChoice(choice.id)}
                        className="sr-only"
                      />
                      <span className={`font-mono text-xs mt-0.5 ${selectedChoice === choice.id ? 'text-accent-hover' : 'text-ink-faint'}`}>
                        {choice.id.toUpperCase()}
                      </span>
                      {choice.text}
                    </label>
                  ))}
                </div>
                <button
                  onClick={handleSubmitChoice}
                  disabled={!selectedChoice || verifying}
                  className="btn-primary w-full mt-3 text-sm py-2.5 disabled:opacity-40 disabled:shadow-none disabled:hover:translate-y-0"
                >
                  {verifying ? '판정 중…' : '답안 제출'}
                </button>
              </div>
            )}

            {/* hint */}
            {problem.hint && (
              <div className="border border-edge bg-surface">
                <button
                  onClick={() => setShowHint(!showHint)}
                  aria-expanded={showHint}
                  className="w-full flex items-center gap-2 px-4 py-3 text-sm text-ink-muted hover:text-ink transition-colors"
                >
                  <Lightbulb className="w-4 h-4 text-warning" aria-hidden="true" />
                  힌트 보기
                  <span className="ml-auto micro text-ink-faint">{showHint ? '접기' : '펼치기'}</span>
                </button>
                {showHint && (
                  <div className="px-4 pb-4 text-sm text-ink-muted leading-relaxed whitespace-pre-line border-t border-edge-soft pt-3 mx-0">
                    {problem.hint}
                  </div>
                )}
              </div>
            )}

            {/* actions */}
            {session && (
              <div className="flex gap-2 pt-1">
                {(problem.verify_type === 'script' || problem.verify_type === 'text') && (
                  <button
                    onClick={handleVerify}
                    disabled={verifying || crashed || status !== 'ready'}
                    className="flex-1 inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-success/40 bg-success-soft text-success hover:bg-success/20 disabled:opacity-40 text-sm font-display font-semibold tracking-wide transition-colors"
                  >
                    <CheckCircle2 className="w-4 h-4" aria-hidden="true" />
                    {verifying ? '검증 중…' : '검증'}
                  </button>
                )}
                <button
                  onClick={handleReset}
                  className="inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-edge bg-surface-2 hover:bg-surface-3 text-sm transition-colors"
                >
                  <RotateCcw className="w-3.5 h-3.5" aria-hidden="true" /> 리셋
                </button>
                <button
                  onClick={handleEnd}
                  className="inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-edge bg-surface-2 hover:border-danger/40 hover:bg-danger-soft hover:text-danger text-sm transition-colors"
                >
                  <Square className="w-3.5 h-3.5" aria-hidden="true" /> 종료
                </button>
              </div>
            )}

            {error && (
              <div className="border border-danger/40 bg-danger-soft p-3 text-sm text-danger">{error}</div>
            )}
          </div>
        </aside>

        {/* ---- terminal ---- */}
        <section className="flex-1 min-w-0 bg-terminal relative">
          {session ? (
            <Terminal />
          ) : (
            <div className="h-full flex items-center justify-center">
              <div className="text-center max-w-sm px-6">
                <div className="font-mono text-left border border-edge bg-surface p-4 text-xs text-ink-faint leading-relaxed">
                  <p><span className="text-accent-hover">$</span> kubectl get pods</p>
                  <p className="text-danger">No resources found — cluster offline.</p>
                  <p className="mt-2 text-ink-muted">환경을 시작하면 이곳에 터미널이 열립니다.</p>
                </div>
              </div>
            </div>
          )}
        </section>
      </div>
    </div>
  )
}
