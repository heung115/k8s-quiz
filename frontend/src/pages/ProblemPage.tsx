import { useEffect, useState, useCallback, useRef } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { api, APIResponseError } from '../api/client'
import { Problem, Session } from '../types'
import { useSessionStore } from '../stores/session'
import { useAuthStore } from '../stores/auth'
import {
  clearPendingStart,
  getOrCreatePendingStart,
  isValidStartResponse,
  loadPendingStart,
} from '../lib/startOperation'
import {
  acknowledgePendingReset,
  clearPendingReset,
  clearPendingResetAfterLifecycle,
  getOrCreatePendingReset,
  isValidResetResponse,
  loadPendingResetForProblem,
  PendingResetOperation,
} from '../lib/resetOperation'
import { VerifyOperationController } from '../lib/verifyOperation'
import { canApplyChoiceResult, ChoiceOperationController } from '../lib/choiceOperation'
import {
  canApplyEndResult,
  EndOperationController,
  EndOperationScope,
  isDefinitiveEndRejection,
  loadPendingEnd,
  loadPendingEndForProblem,
  PendingEndOperation,
} from '../lib/endOperation'
import { useLifecycleStream } from '../hooks/useLifecycleStream'
import { Terminal } from '../components/Terminal'
import { SevTag, TypeTag, categoryMeta } from '../components/ui'
import {
  Lightbulb, RotateCcw, CheckCircle2, XCircle, Play, Square,
  AlertTriangle, ChevronLeft, Timer, Wifi, WifiOff,
} from 'lucide-react'

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : '오류가 발생했습니다'
}

function useCountdown(timeoutAt: string | null | undefined, active: boolean) {
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

interface PageRequestScope {
  key: string
  epoch: symbol
}

type MutationKind = 'start' | 'reset' | 'end' | 'verify' | 'choice'

interface PageMutation {
  kind: MutationKind
  token: symbol
}

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
              <span className={`w-2.5 h-2.5 mt-1 shrink-0 ${crashed ? 'bg-danger' : done ? 'bg-success' : active ? 'bg-accent-hover animate-pulse-dot' : 'bg-edge'}`} aria-hidden="true" />
              {i < steps.length - 1 && <span className={`w-px flex-1 my-1 ${done ? 'bg-success/50' : 'bg-edge-soft'}`} aria-hidden="true" />}
            </div>
            <div className="pb-4">
              <p className={`text-sm font-medium leading-tight ${done ? 'text-ink' : active ? 'text-accent-hover' : 'text-ink-faint'}`}>
                {s.label}{active && <span className="ml-2 micro text-accent-hover">진행 중…</span>}
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
  const [resetting, setResetting] = useState(false)
  const [ending, setEnding] = useState(false)
  const [verifying, setVerifying] = useState(false)
  const [error, setError] = useState('')
  const [showHint, setShowHint] = useState(false)
  const [selectedChoice, setSelectedChoice] = useState('')
  const verifyOperationRef = useRef(new VerifyOperationController())
  const choiceOperationRef = useRef(new ChoiceOperationController())
  const endOperationRef = useRef(new EndOperationController())
  const startTokenRef = useRef<symbol | null>(null)
  const resetTokenRef = useRef<symbol | null>(null)
  const endTokenRef = useRef<symbol | null>(null)
  const gradingTokenRef = useRef<symbol | null>(null)
  const mutationRef = useRef<PageMutation | null>(null)
  const pageScopeRef = useRef<PageRequestScope>({ key: '', epoch: Symbol('initial-page') })
  const user = useAuthStore((state) => state.user)
  const userID = user?.id
  const pageScopeKey = userID && id ? `${userID}\0${id}` : ''
  if (pageScopeRef.current.key !== pageScopeKey) {
    pageScopeRef.current = { key: pageScopeKey, epoch: Symbol(pageScopeKey) }
    startTokenRef.current = null
    resetTokenRef.current = null
    endTokenRef.current = null
    gradingTokenRef.current = null
    mutationRef.current = null
  }
  const {
    session,
    crashed,
    verifyResult,
    wsConnected,
    notice,
    applyStartSnapshot,
    applyResetSnapshot,
    setCrashed,
    setVerifyResult,
    clear,
  } = useSessionStore()
  const endIntent = useSessionStore((state) => state.endIntent)
  useLifecycleStream(userID, id)

  const remaining = useCountdown(session?.timeout_at, !!session)
  const isUrgent = remaining !== null && remaining <= 300

  const isCurrentPage = useCallback((scope: PageRequestScope, expectedUserID: string) =>
    pageScopeRef.current === scope && useAuthStore.getState().user?.id === expectedUserID, [])

  const acquireMutation = useCallback((kind: MutationKind): symbol | null => {
    if (mutationRef.current) return null
    const token = Symbol(kind)
    mutationRef.current = { kind, token }
    return token
  }, [])

  const ownsMutation = useCallback((kind: MutationKind, token: symbol): boolean =>
    mutationRef.current?.kind === kind && mutationRef.current.token === token, [])

  const releaseMutation = useCallback((kind: MutationKind, token: symbol): void => {
    if (ownsMutation(kind, token)) mutationRef.current = null
  }, [ownsMutation])

  const runStart = useCallback(async () => {
    if (!id || !userID) return false
    const problemID = id
    const expectedUserID = userID
    const scope = pageScopeRef.current
    if (!isCurrentPage(scope, expectedUserID)) return false
    if (useSessionStore.getState().endIntent?.problemId === problemID) return false
    const mutationToken = acquireMutation('start')
    if (!mutationToken) return false
    startTokenRef.current = mutationToken
    const isCurrent = () => isCurrentPage(scope, expectedUserID) &&
      startTokenRef.current === mutationToken && ownsMutation('start', mutationToken)
    setStarting(true)
    setError('')
    const pending = getOrCreatePendingStart(expectedUserID, problemID)
    const authorityEpoch = useSessionStore.getState().authorityEpoch
    try {
      const data: unknown = await api.post<Session>(
        `/api/problems/${problemID}/start`,
        undefined,
        { 'Idempotency-Key': pending.key },
      )
      if (!isCurrent()) return false
      if (useSessionStore.getState().endIntent?.problemId === problemID) return false
      if (!isValidStartResponse(data, problemID, pending.key)) {
        throw new Error('서버가 올바른 세션 응답을 반환하지 않았습니다. 같은 요청으로 다시 시도합니다.')
      }
      clearPendingStart(expectedUserID, problemID, pending.key)
      if (!applyStartSnapshot(data, authorityEpoch)) {
        throw new Error('더 최신 세션 상태가 확인되어 이전 시작 응답을 적용하지 않았습니다.')
      }
      return true
    } catch (err) {
      if (!isCurrent()) return false
      // A concrete non-retryable 4xx proves that this request was rejected.
      // 429 is retryable and every transport/5xx/protocol failure is
      // ambiguous, so those retain the exact key for replay.
      if (err instanceof APIResponseError && err.status >= 400 && err.status < 500 && err.status !== 429) {
        clearPendingStart(expectedUserID, problemID, pending.key)
      }
      setError(errMsg(err))
      return false
    } finally {
      if (isCurrent()) {
        startTokenRef.current = null
        setStarting(false)
      }
      releaseMutation('start', mutationToken)
    }
  }, [acquireMutation, applyStartSnapshot, id, isCurrentPage, ownsMutation, releaseMutation, userID])

  const runReset = useCallback(async (existing?: PendingResetOperation) => {
    const current = useSessionStore.getState().session
    if (!id || !userID || (!existing && (!current || current.cleanup_pending === true))) return false
    const problemID = id
    const expectedUserID = userID
    const scope = pageScopeRef.current
    if (!isCurrentPage(scope, expectedUserID)) return false
    if (existing && (existing.userId !== expectedUserID || existing.problemId !== problemID)) return false
    if (!existing && current?.problem_id !== problemID) return false
    const mutationToken = acquireMutation('reset')
    if (!mutationToken) return false
    const pending = existing || getOrCreatePendingReset(
      expectedUserID,
      problemID,
      current!.session_id,
      current!.generation,
    )
    const endBlocksReset = () => {
      const intent = useSessionStore.getState().endIntent
      return intent?.problemId === problemID && intent.sessionId === pending.sessionId &&
        (intent.generation === pending.sourceGeneration || intent.generation === pending.sourceGeneration + 1)
    }
    if (endBlocksReset()) {
      releaseMutation('reset', mutationToken)
      return false
    }
    resetTokenRef.current = mutationToken
    const isCurrent = () => isCurrentPage(scope, expectedUserID) &&
      resetTokenRef.current === mutationToken && ownsMutation('reset', mutationToken)
    setResetting(true)
    setError('')
    const authorityEpoch = useSessionStore.getState().authorityEpoch
    try {
      const data: unknown = await api.post<Session>(
        `/api/problems/${problemID}/reset`,
        { session_id: pending.sessionId, source_generation: pending.sourceGeneration },
        { 'Idempotency-Key': pending.key },
      )
      if (!isCurrent()) return false
      if (endBlocksReset()) return false
      if (!isValidResetResponse(data, pending)) {
        throw new Error('서버가 올바른 리셋 응답을 반환하지 않았습니다. 같은 요청으로 다시 시도합니다.')
      }
      if (!acknowledgePendingReset(pending, data)) {
        throw new Error('리셋 작업 상태를 안전하게 저장하지 못했습니다.')
      }
      if (!existing && !applyResetSnapshot(
        data,
        pending.sessionId,
        pending.sourceGeneration,
        authorityEpoch,
      )) {
        throw new Error('더 최신 세션 상태가 확인되어 이전 리셋 응답을 적용하지 않았습니다.')
      }
      const authoritative = useSessionStore.getState()
      if (authoritative.lifecycleAuthoritative && authoritative.session) {
        clearPendingResetAfterLifecycle(expectedUserID, authoritative.session)
      }
      setVerifyResult(null)
      setCrashed(false)
      return true
    } catch (err) {
      if (!isCurrent()) return false
      // 400/401/403/404/409 prove that this exact precondition was rejected.
      // 429, 5xx, transport failures, and malformed 2xx responses remain
      // ambiguous and must replay the same operation key.
      if (err instanceof APIResponseError && err.status >= 400 && err.status < 500 && err.status !== 429) {
        clearPendingReset(pending)
      }
      setError(errMsg(err))
      return false
    } finally {
      if (isCurrent()) {
        resetTokenRef.current = null
        setResetting(false)
      }
      releaseMutation('reset', mutationToken)
    }
  }, [acquireMutation, applyResetSnapshot, id, isCurrentPage, ownsMutation, releaseMutation, setCrashed, setVerifyResult, userID])

  const runEnd = useCallback(async (existing?: PendingEndOperation) => {
    const current = useSessionStore.getState().session
    if (!id || !userID || current?.status === 'verifying' || (!existing && !current)) return false
    const problemID = id
    const expectedUserID = userID
    const pageScope = pageScopeRef.current
    if (!isCurrentPage(pageScope, expectedUserID)) return false
    const expected: EndOperationScope = existing || {
      userId: expectedUserID,
      problemId: problemID,
      sessionId: current!.session_id,
      generation: current!.generation,
    }
    if (expected.userId !== expectedUserID || expected.problemId !== problemID ||
      (!existing && current?.problem_id !== problemID)) return false

    const mutationToken = acquireMutation('end')
    if (!mutationToken) return false

    const authority = useSessionStore.getState()
    if (!authority.beginEndIntent({
      problemId: expected.problemId,
      sessionId: expected.sessionId,
      generation: expected.generation,
    }, authority.authoritativeNull)) {
      releaseMutation('end', mutationToken)
      return false
    }

    endTokenRef.current = mutationToken
    const pageIsCurrent = () => isCurrentPage(pageScope, expectedUserID) &&
      endTokenRef.current === mutationToken && ownsMutation('end', mutationToken)
    setEnding(true)
    setError('')
    try {
      const result = await endOperationRef.current.execute(expected, (operationID) =>
        api.post<unknown>(
          '/api/sessions/end',
          { session_id: expected.sessionId, generation: expected.generation },
          { 'Idempotency-Key': operationID },
        ),
      )
      if (result.state === 'completed') {
        // Completion is operation-scoped, not page-scoped. Clear the exact
        // durable overlay even if this page unmounted while the 204 was in
        // flight; page UI and navigation remain fenced below.
        useSessionStore.getState().clearEndIntent(expected.sessionId, expected.generation)
      }
      if (!pageIsCurrent()) return false
      const latest = useSessionStore.getState()
      const canApply = canApplyEndResult(latest.session, expected, result, latest.authoritativeNull)
      if (result.state === 'pending') {
        return canApply && latest.session !== null
          ? latest.applyEndPending(expected.sessionId, expected.generation)
          : false
      }
      if (!canApply) return false
      clear()
      navigate('/')
      return true
    } catch (err) {
      if (isDefinitiveEndRejection(err)) {
        useSessionStore.getState().clearEndIntent(expected.sessionId, expected.generation)
      }
      if (!pageIsCurrent()) return false
      const latest = useSessionStore.getState().session
      const stillRelevant = latest === null || (latest.problem_id === expected.problemId &&
        latest.session_id === expected.sessionId && latest.generation === expected.generation)
      if (stillRelevant) setError(errMsg(err))
      return false
    } finally {
      if (pageIsCurrent()) {
        endTokenRef.current = null
        setEnding(false)
      }
      releaseMutation('end', mutationToken)
    }
  }, [acquireMutation, clear, id, isCurrentPage, navigate, ownsMutation, releaseMutation, userID])

  const restoreSession = useCallback(async () => {
    if (!id || !userID) return
    const problemID = id
    const expectedUserID = userID
    const scope = pageScopeRef.current
    if (!isCurrentPage(scope, expectedUserID)) return
    const pendingEnd = loadPendingEndForProblem(expectedUserID, problemID)
    if (!pendingEnd && loadPendingStart(expectedUserID, problemID)) {
      await runStart()
      return
    }
    const pendingReset = pendingEnd ? null : loadPendingResetForProblem(expectedUserID, problemID)
    if (pendingReset) {
      await runReset(pendingReset)
      // A historical replay acknowledges its own generation, while the
      // server may already be newer. Always follow reload replay with the
      // authoritative current-session snapshot.
    }
    if (!isCurrentPage(scope, expectedUserID)) return
    const authorityEpoch = useSessionStore.getState().beginAuthorityRead()
    let currentConfirmed = false
    try {
      const data: unknown = await api.get<unknown>('/api/sessions/current')
      if (!isCurrentPage(scope, expectedUserID)) return
      currentConfirmed = useSessionStore.getState().applyCurrentHTTPSnapshot(data, authorityEpoch)
      if (!currentConfirmed) {
        const state = useSessionStore.getState()
        currentConfirmed = state.authorityEpoch !== authorityEpoch && state.lifecycleAuthoritative
        if (!currentConfirmed && state.authorityEpoch === authorityEpoch) {
          setError('서버가 올바른 현재 세션 응답을 반환하지 않았습니다.')
        }
      }
    } catch (err) {
      if (isCurrentPage(scope, expectedUserID) && err instanceof APIResponseError && err.status === 404) {
        currentConfirmed = useSessionStore.getState().applyCurrentHTTPNotFound(authorityEpoch)
        if (!currentConfirmed) {
          const state = useSessionStore.getState()
          currentConfirmed = state.authorityEpoch !== authorityEpoch && state.lifecycleAuthoritative
        }
      } else if (isCurrentPage(scope, expectedUserID)) {
        const state = useSessionStore.getState()
        currentConfirmed = state.authorityEpoch !== authorityEpoch && state.lifecycleAuthoritative
        setError(errMsg(err))
      }
    }
    if (!isCurrentPage(scope, expectedUserID) || !currentConfirmed) return
    if (pendingEnd) await runEnd(pendingEnd)
  }, [id, isCurrentPage, runEnd, runReset, runStart, userID])

  useEffect(() => {
    if (!id || !userID) return
    const scope: PageRequestScope = { key: `${userID}\0${id}`, epoch: Symbol(`${userID}\0${id}`) }
    pageScopeRef.current = scope
    startTokenRef.current = null
    resetTokenRef.current = null
    endTokenRef.current = null
    gradingTokenRef.current = null
    mutationRef.current = null
    setProblem(null)
    setStarting(false)
    setResetting(false)
    setEnding(false)
    api.get<Problem>(`/api/problems/${id}`)
      .then((value) => { if (isCurrentPage(scope, userID)) setProblem(value) })
      .catch(() => { if (isCurrentPage(scope, userID)) setError('문제를 찾을 수 없습니다.') })
    void restoreSession()
    return () => {
      if (pageScopeRef.current === scope) {
        pageScopeRef.current = { key: '', epoch: Symbol('inactive-page') }
        startTokenRef.current = null
        resetTokenRef.current = null
        endTokenRef.current = null
        gradingTokenRef.current = null
        mutationRef.current = null
      }
    }
  }, [id, userID, restoreSession, isCurrentPage])

  useEffect(() => {
    // server_restart clears the session and sets a notice (Layout shows it);
    // route back to the dashboard so the dead terminal isn't left mounted.
    if (notice) navigate('/')
  }, [notice, navigate])

  useEffect(() => {
    if (session && id && session.problem_id !== id) {
      navigate(`/problems/${session.problem_id}`, { replace: true })
    }
  }, [id, navigate, session])

  useEffect(() => {
    verifyOperationRef.current.reset()
    choiceOperationRef.current.reset()
    // A lifecycle null or replacement may arrive before an exact HTTP
    // mutation settles. Keep the owning token/UI flag until its own finally
    // block runs; late responses are fenced by exact authority below.
    if (mutationRef.current?.kind !== 'verify' && mutationRef.current?.kind !== 'choice') {
      setVerifying(false)
    }
    if (mutationRef.current?.kind !== 'end') setEnding(false)
  }, [session?.session_id, session?.generation])

  const handleStart = () => { void runStart() }
  const handleReset = () => { void runReset() }
  const handleVerify = async () => {
    const currentSession = useSessionStore.getState().session
    const currentEndIntent = useSessionStore.getState().endIntent
    if (!id || !userID || !currentSession || currentSession.cleanup_pending === true || currentEndIntent) return
    const gradingToken = acquireMutation('verify')
    if (!gradingToken) return
    gradingTokenRef.current = gradingToken
    setVerifying(true); setVerifyResult(null)
    const expectedSession = { sessionID: currentSession.session_id, generation: currentSession.generation }
    const isCurrentVerifySession = () => {
      const current = useSessionStore.getState().session
      const pendingEnd = useSessionStore.getState().endIntent
      return current?.session_id === expectedSession.sessionID && current.generation === expectedSession.generation &&
        !(pendingEnd?.sessionId === expectedSession.sessionID && pendingEnd.generation === expectedSession.generation)
    }
    try {
      const result = await verifyOperationRef.current.execute({
        userId: userID,
        problemId: id,
        sessionId: currentSession.session_id,
        generation: currentSession.generation,
      }, (operationID) =>
        api.post<{ success: boolean; log: string }>(
          `/api/problems/${id}/verify`,
          undefined,
          { 'Idempotency-Key': operationID },
        ),
      )
      if (isCurrentVerifySession()) setVerifyResult(result)
    } catch (err) {
      if (isCurrentVerifySession()) setError(errMsg(err))
    }
    finally {
      if (gradingTokenRef.current === gradingToken) {
        gradingTokenRef.current = null
        setVerifying(false)
      }
      releaseMutation('verify', gradingToken)
    }
  }
  const handleSubmitChoice = async () => {
    const currentSession = useSessionStore.getState().session
    const currentEndIntent = useSessionStore.getState().endIntent
    if (!selectedChoice || !currentSession || currentSession.cleanup_pending === true || currentEndIntent || !id || !userID) return
    const gradingToken = acquireMutation('choice')
    if (!gradingToken) return
    gradingTokenRef.current = gradingToken
    const expected = {
      userId: userID,
      problemId: id,
      sessionId: currentSession.session_id,
      generation: currentSession.generation,
      choiceId: selectedChoice,
    }
    const currentChoiceScope = () => {
      const current = useSessionStore.getState().session
      const pageCurrent = pageScopeRef.current.key === `${expected.userId}\0${expected.problemId}` &&
        useAuthStore.getState().user?.id === expected.userId
      const sessionCurrent = current?.problem_id === expected.problemId &&
        current.session_id === expected.sessionId && current.generation === expected.generation
      const pendingEnd = useSessionStore.getState().endIntent
      const endCurrent = pendingEnd?.sessionId === expected.sessionId && pendingEnd.generation === expected.generation
      return { pageCurrent, sessionCurrent: sessionCurrent && !endCurrent, noSession: current === null && !endCurrent }
    }
    setVerifying(true); setVerifyResult(null); setError('')
    try {
      const data = await choiceOperationRef.current.execute(expected, (operationID) =>
        api.post<unknown>(
          `/api/problems/${expected.problemId}/submit`,
          { session_id: expected.sessionId, generation: expected.generation, choice_id: expected.choiceId },
          { 'Idempotency-Key': operationID },
        ),
      )
      const current = currentChoiceScope()
      // A null or replacement authority cannot prove that this response
      // caused the transition. Apply grades only while the exact allocation
      // remains current and no exact end intent owns it.
      const authority = useSessionStore.getState()
      const exactEnd = authority.endIntent?.problemId === expected.problemId &&
        authority.endIntent.sessionId === expected.sessionId && authority.endIntent.generation === expected.generation
      if (current.pageCurrent && !exactEnd && canApplyChoiceResult(authority.session, expected, data)) {
        setVerifyResult({ success: data.success, log: '' })
      }
    } catch (err) {
      const current = currentChoiceScope()
      if (current.pageCurrent && current.sessionCurrent) setError(errMsg(err))
    } finally {
      const current = currentChoiceScope()
      if (gradingTokenRef.current === gradingToken) {
        gradingTokenRef.current = null
        if (current.pageCurrent && (current.sessionCurrent || current.noSession)) setVerifying(false)
      }
      releaseMutation('choice', gradingToken)
    }
  }
  const handleEnd = () => {
    if (!session && endIntent && userID && id && endIntent.problemId === id) {
      const pending = loadPendingEnd({
        userId: userID,
        problemId: id,
        sessionId: endIntent.sessionId,
        generation: endIntent.generation,
      })
      if (pending) void runEnd(pending)
      return
    }
    void runEnd()
  }

  if (!problem) {
    return <div className="max-w-7xl mx-auto px-5 py-10 micro text-ink-faint">LOADING SCENARIO<span className="animate-blink">▍</span></div>
  }

  const cat = categoryMeta[problem.category]
  const CatIcon = cat?.icon
  const isChoice = problem.verify_type === 'choice'
  const status = session?.status || 'idle'
  const endIntentForPage = !!endIntent && endIntent.problemId === id && (!session || (
    endIntent.sessionId === session.session_id && endIntent.generation === session.generation
  ))
  const cleanupLocked = session?.cleanup_pending === true || status === 'destroying' || !!endIntentForPage
  const terminalReady = status === 'ready' && !cleanupLocked

  return (
    <div className="min-h-[calc(100vh-3.5rem)] lg:h-[calc(100vh-3.5rem)] flex flex-col">
      {/* incident header */}
      <div className="border-b border-edge bg-surface px-4 sm:px-5 py-3 flex items-center gap-3 sm:gap-4 flex-wrap">
        <Link to="/problems" className="text-ink-faint hover:text-ink transition-colors p-1 -ml-1" aria-label="문제 목록으로">
          <ChevronLeft className="w-5 h-5" aria-hidden="true" />
        </Link>
        <div className="flex items-center gap-3 min-w-0">
          <h1 className="font-display font-semibold text-base sm:text-lg truncate">{problem.title}</h1>
          <div className="hidden sm:flex items-center gap-2">
            <SevTag difficulty={problem.difficulty} />
            <TypeTag type={problem.type} />
            <span className="micro text-ink-faint inline-flex items-center gap-1.5">
              {CatIcon && <CatIcon className="w-3.5 h-3.5" aria-hidden="true" />}{cat?.label || problem.category}
            </span>
          </div>
        </div>
        <div className="ml-auto flex items-center gap-3 sm:gap-4">
          {session && terminalReady && !crashed && (
            <span className={`hidden sm:inline-flex items-center gap-1.5 micro ${wsConnected ? 'text-success' : 'text-ink-faint'}`}>
              {wsConnected ? <Wifi className="w-3.5 h-3.5" aria-hidden="true" /> : <WifiOff className="w-3.5 h-3.5" aria-hidden="true" />}
              {wsConnected ? 'TTY LINKED' : 'TTY OFFLINE'}
            </span>
          )}
          {session && remaining !== null && (
            <span className={`inline-flex items-center gap-2 font-mono text-sm px-3 py-1.5 border ${isUrgent ? 'border-danger/50 bg-danger-soft text-danger animate-pulse' : 'border-edge bg-surface-2 text-ink'}`} role="timer" aria-label="남은 시간">
              <Timer className="w-4 h-4" aria-hidden="true" />{formatTime(remaining)}
            </span>
          )}
        </div>
      </div>

      {/* body: stacks on mobile, two columns on lg+ */}
      <div className="flex-1 flex flex-col lg:flex-row lg:min-h-0">
        <aside className="w-full lg:w-[380px] lg:shrink-0 border-b lg:border-b-0 lg:border-r border-edge bg-canvas lg:overflow-y-auto">
          <div className="p-5 space-y-5">
            <div>
              <p className="micro text-ink-faint mb-2"><span className="text-accent">//</span> INCIDENT BRIEF</p>
              <p className="text-sm text-ink-muted leading-relaxed whitespace-pre-line">{problem.description}</p>
            </div>

            {!session ? (
              <div className="border border-edge bg-surface p-4">
                <p className="text-sm text-ink-muted leading-relaxed">
                  환경을 시작하면 격리된 k3s 클러스터가 부팅되고, 터미널에서 <span className="font-mono text-info">kubectl</span>로 문제를 진단할 수 있습니다.
                </p>
                <button onClick={handleStart} disabled={starting || cleanupLocked} className="btn-primary w-full mt-4 text-sm py-2.5">
                  <Play className="w-4 h-4" aria-hidden="true" />{starting ? '환경 준비 중…' : '환경 시작'}
                </button>
                {starting && <p className="micro text-accent-hover mt-3 text-center">컨테이너 생성 → k3s 부팅 → 시나리오 주입</p>}
              </div>
            ) : (
              <div className="border border-edge bg-surface p-4">
                <p className="micro text-ink-faint mb-3"><span className="text-accent">//</span> ENVIRONMENT</p>
                <StageTimeline status={crashed ? 'failed' : status} crashed={crashed} />
              </div>
            )}

            {crashed && (
              <div className="border border-danger/40 bg-danger-soft p-4">
                <p className="flex items-center gap-2 text-sm font-semibold text-danger"><AlertTriangle className="w-4 h-4" aria-hidden="true" /> 컨테이너가 중단되었습니다</p>
                <p className="mt-1 text-xs text-danger/80">리셋으로 환경을 다시 띄우세요. 이전 작업은 사라집니다.</p>
              </div>
            )}

            {cleanupLocked && (
              <div className="border border-warning/40 bg-warning/10 p-4">
                <p className="text-sm font-semibold text-warning">실행 환경을 정리하고 있습니다</p>
                <p className="mt-1 text-xs text-ink-muted">정리가 확인될 때까지 터미널과 변경 작업이 잠깁니다. 종료 버튼으로 상태를 다시 확인할 수 있습니다.</p>
              </div>
            )}

            {verifyResult && (
              <div className={`border ${verifyResult.success ? 'border-success/40' : 'border-danger/40'} bg-terminal`}>
                <div className={`px-3 py-2 border-b flex items-center gap-2 ${verifyResult.success ? 'border-success/30' : 'border-danger/30'}`}>
                  {verifyResult.success ? <CheckCircle2 className="w-4 h-4 text-success" aria-hidden="true" /> : <XCircle className="w-4 h-4 text-danger" aria-hidden="true" />}
                  <span className={`micro ${verifyResult.success ? 'text-success' : 'text-danger'}`}>{verifyResult.success ? 'VERIFICATION PASSED' : 'VERIFICATION FAILED'}</span>
                </div>
                <div className="p-3 font-mono text-xs leading-relaxed">
                  {verifyResult.success ? <p className="text-success">✓ 시나리오 클리어! 프로필에 기록되었습니다.</p> : <p className="text-danger">✗ 아직 조건을 만족하지 않습니다. 터미널에서 상태를 다시 확인하세요.</p>}
                  {verifyResult.log && <pre className="mt-2 text-ink-muted whitespace-pre-wrap overflow-x-auto">{verifyResult.log}</pre>}
                </div>
              </div>
            )}

            {isChoice && session && problem.choices && (
              <div className="border border-edge bg-surface p-4">
                <p className="micro text-ink-faint mb-3"><span className="text-accent">//</span> 원인 선택</p>
                <div className="space-y-2" role="radiogroup" aria-label="원인 선택">
                  {problem.choices.map((choice) => (
                    <label key={choice.id} className={`flex items-start gap-3 border p-3 cursor-pointer transition-colors text-sm focus-within:border-accent/70 focus-within:bg-accent-dim ${selectedChoice === choice.id ? 'border-accent/60 bg-accent-dim text-ink' : 'border-edge text-ink-muted hover:bg-surface-2'}`}>
                      <input type="radio" name="choice" value={choice.id} checked={selectedChoice === choice.id} onChange={() => setSelectedChoice(choice.id)} disabled={cleanupLocked} className="sr-only" />
                      <span className={`font-mono text-xs mt-0.5 ${selectedChoice === choice.id ? 'text-accent-hover' : 'text-ink-faint'}`}>{choice.id.toUpperCase()}</span>
                      {choice.text}
                    </label>
                  ))}
                </div>
                <button onClick={handleSubmitChoice} disabled={!selectedChoice || verifying || cleanupLocked} className="btn-primary w-full mt-3 text-sm py-2.5 disabled:opacity-40 disabled:shadow-none disabled:hover:translate-y-0">
                  {verifying ? '판정 중…' : '답안 제출'}
                </button>
              </div>
            )}

            {problem.hint && (
              <div className="border border-edge bg-surface">
                <button onClick={() => setShowHint(!showHint)} aria-expanded={showHint} className="w-full flex items-center gap-2 px-4 py-3 text-sm text-ink-muted hover:text-ink transition-colors">
                  <Lightbulb className="w-4 h-4 text-warning" aria-hidden="true" />힌트 보기
                  <span className="ml-auto micro text-ink-faint">{showHint ? '접기' : '펼치기'}</span>
                </button>
                {showHint && <div className="px-4 pb-4 text-sm text-ink-muted leading-relaxed whitespace-pre-line border-t border-edge-soft pt-3">{problem.hint}</div>}
              </div>
            )}

            {(session || endIntentForPage) && (
              <div className="flex gap-2 pt-1">
                {session && (problem.verify_type === 'script' || problem.verify_type === 'text') && (
                  <button onClick={handleVerify} disabled={verifying || crashed || !terminalReady} className="flex-1 inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-success/40 bg-success-soft text-success hover:bg-success/20 disabled:opacity-40 text-sm font-display font-semibold tracking-wide transition-colors">
                    <CheckCircle2 className="w-4 h-4" aria-hidden="true" />{verifying ? '검증 중…' : '검증'}
                  </button>
                )}
                {session && <button onClick={handleReset} disabled={resetting || cleanupLocked} className="inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-edge bg-surface-2 hover:bg-surface-3 disabled:opacity-40 text-sm transition-colors"><RotateCcw className={`w-3.5 h-3.5 ${resetting ? 'animate-spin' : ''}`} aria-hidden="true" /> {resetting ? '리셋 중…' : '리셋'}</button>}
                <button onClick={handleEnd} disabled={ending || resetting || verifying} className="inline-flex items-center justify-center gap-1.5 px-4 py-2.5 border border-edge bg-surface-2 hover:border-danger/40 hover:bg-danger-soft hover:text-danger disabled:opacity-40 text-sm transition-colors"><Square className="w-3.5 h-3.5" aria-hidden="true" /> {ending ? '확인 중…' : cleanupLocked ? '정리 상태 확인' : '종료'}</button>
              </div>
            )}

            {error && <div className="border border-danger/40 bg-danger-soft p-3 text-sm text-danger">{error}</div>}
          </div>
        </aside>

        <section className="bg-terminal relative h-[60vh] lg:h-auto lg:flex-1 lg:min-h-0">
          {session && terminalReady && !crashed ? (
	            <Terminal
	              key={`${session.session_id}:${session.generation}`}
	              sessionId={session.session_id}
	              generation={session.generation}
	            />
          ) : session ? (
            <div className="h-full flex items-center justify-center">
              <div className="text-center max-w-sm px-6">
                <div className="font-mono text-left border border-edge bg-surface p-4 text-xs text-ink-faint leading-relaxed">
                  <p><span className="text-accent-hover">$</span> waiting for environment</p>
                  <p className="text-warning">Terminal remains locked until the Runner reports ready.</p>
                  <p className="mt-2 text-ink-muted">클러스터 준비가 끝나면 자동으로 연결됩니다.</p>
                </div>
              </div>
            </div>
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
