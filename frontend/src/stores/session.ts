import { create } from 'zustand'
import {
  LIFECYCLE_SCHEMA,
  LIFECYCLE_STATUSES,
  LifecycleCursor,
  LifecycleEventFrame,
  LifecycleServerMessage,
  LifecycleSessionSnapshot,
  LifecycleSnapshotFrame,
  Session,
  WSMessage,
} from '../types'
import { parseSessionSnapshot } from '../lib/sessionSnapshot'

export type LifecycleApplyResult = 'applied' | 'ignored' | 'resync'

export interface EndIntent {
  problemId: string
  sessionId: string
  generation: number
}

interface SessionState {
  session: Session | null
  stage: string
  stageMessage: string
  crashed: boolean
  verifyResult: { success: boolean; log: string } | null
  wsConnected: boolean
  lifecycleCursor: LifecycleCursor | null
  lifecycleConnected: boolean
  lifecycleResyncing: boolean
  lifecycleAuthoritative: boolean
  authorityEpoch: number
  authoritativeNull: boolean
  endIntent: EndIntent | null
  // Transient global notice (e.g. server restart) surfaced by Layout; cleared
  // independently of clear() so it survives leaving the problem page.
  notice: string | null
  setSession: (session: Session | null) => void
  beginAuthorityRead: () => number
  applyStartSnapshot: (session: Session, expectedAuthorityEpoch: number) => boolean
  applyCurrentHTTPSnapshot: (value: unknown, authorityEpoch: number) => boolean
  applyCurrentHTTPNotFound: (authorityEpoch: number) => boolean
  applyResetSnapshot: (
    session: Session,
    sourceSessionId: string,
    sourceGeneration: number,
    expectedAuthorityEpoch: number,
  ) => boolean
  beginEndIntent: (intent: EndIntent, allowMissing?: boolean) => boolean
  clearEndIntent: (sessionId: string, generation: number) => boolean
  applyEndPending: (sessionId: string, generation: number) => boolean
  setStage: (stage: string, message: string) => void
  setCrashed: (crashed: boolean) => void
  setVerifyResult: (result: { success: boolean; log: string } | null) => void
  setWsConnected: (connected: boolean) => void
  setLifecycleConnected: (connected: boolean) => void
  restoreLifecycleCursor: (cursor: LifecycleCursor) => boolean
  prepareLifecycleResync: () => void
  handleLifecycleMessage: (
    msg: unknown,
    expectedProblemId?: string,
    initialSnapshotAuthorityEpoch?: number,
  ) => LifecycleApplyResult
  setNotice: (notice: string | null) => void
  handleWSMessage: (msg: WSMessage) => void
  clear: () => void
}

// Maps backend boot stages (service.go emitStage) onto the session.status the
// UI gates on (ProblemPage verify button + StageTimeline). Stages not listed
// (e.g. container_crashed) leave status untouched so existing handling stands.
const STATUS_BY_STAGE: Partial<Record<string, Session['status']>> = {
  container_created: 'booting',
  k3s_booting: 'booting',
  setup_running: 'setting_up',
  ready: 'ready',
}

// HTTP snapshots and WebSocket stages race each other. Within one allocation,
// delayed boot snapshots may add fresher metadata but may not move the visible
// boot lifecycle backwards. States outside this boot sequence (for example
// verifying -> ready) retain their ordinary server-defined transitions.
const BOOT_STATUS_RANK: Record<string, number> = {
  queued: 0,
  creating: 0,
  provisioning: 0,
  booting: 1,
  setting_up: 2,
  ready: 3,
}

const LIFECYCLE_STATUS_SET = new Set<string>(LIFECYCLE_STATUSES)

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function isPositiveInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
}

function isLifecycleCursor(value: unknown): value is LifecycleCursor {
  if (!isRecord(value)) return false
  return typeof value.session_id === 'string' && value.session_id.length > 0 &&
    isPositiveInteger(value.generation) && isPositiveInteger(value.event_sequence)
}

function isLifecycleSession(value: unknown): value is LifecycleSessionSnapshot {
	if (!isRecord(value)) return false
	const verifyResult = value.latest_verify_result
	const validVerifyResult = verifyResult === null || verifyResult === undefined ||
		(isRecord(verifyResult) && typeof verifyResult.success === 'boolean' && typeof verifyResult.log === 'string')
	return typeof value.session_id === 'string' && value.session_id.length > 0 &&
    typeof value.problem_id === 'string' && value.problem_id.length > 0 &&
    isPositiveInteger(value.generation) &&
    typeof value.operation_id === 'string' && value.operation_id.length > 0 &&
    typeof value.status === 'string' && LIFECYCLE_STATUS_SET.has(value.status) &&
    (value.timeout_at === null || (typeof value.timeout_at === 'string' && Number.isFinite(Date.parse(value.timeout_at)))) &&
    typeof value.cleanup_pending === 'boolean' &&
		(value.terminal_reason === null || typeof value.terminal_reason === 'string') &&
		validVerifyResult
}

function isLifecycleSnapshot(value: unknown): value is LifecycleSnapshotFrame {
  if (!isRecord(value) || value.type !== 'lifecycle_snapshot' || value.schema !== LIFECYCLE_SCHEMA) return false
  if (value.session === null || value.cursor === null) return value.session === null && value.cursor === null
  if (!isLifecycleSession(value.session) || !isLifecycleCursor(value.cursor)) return false
  return value.session.session_id === value.cursor.session_id &&
    value.session.generation === value.cursor.generation
}

function isLifecycleEvent(value: unknown): value is LifecycleEventFrame {
  if (!isRecord(value) || value.type !== 'lifecycle_event' || value.schema !== LIFECYCLE_SCHEMA) return false
  return typeof value.session_id === 'string' && value.session_id.length > 0 &&
    isPositiveInteger(value.generation) && isPositiveInteger(value.event_sequence) &&
    typeof value.event_type === 'string' && value.event_type.length > 0 &&
    typeof value.reason_code === 'string' && typeof value.message === 'string' &&
    typeof value.occurred_at === 'string' && Number.isFinite(Date.parse(value.occurred_at)) &&
    isRecord(value.payload)
}

function classifyLifecycleMessage(value: unknown): LifecycleServerMessage | null {
  if (isLifecycleSnapshot(value) || isLifecycleEvent(value)) return value
  if (isRecord(value) && value.type === 'lifecycle_resync_required' && typeof value.reason === 'string') {
    return { type: 'lifecycle_resync_required', reason: value.reason }
  }
  return null
}

function lifecycleStage(status: string): string {
  switch (status) {
    case 'queued':
    case 'provisioning':
      return 'container_created'
    case 'booting':
      return 'k3s_booting'
    case 'setting_up':
      return 'setup_running'
    case 'ready':
      return 'ready'
    case 'verifying':
      return 'verifying'
    case 'failed':
    case 'timed_out':
    case 'provider_lost':
    case 'destroying':
    case 'destroyed':
    case 'completed':
      return status
    default:
      return ''
  }
}

function sessionFromLifecycle(snapshot: LifecycleSessionSnapshot, cursor: LifecycleCursor): Session {
  return {
    operation_id: snapshot.operation_id,
    session_id: snapshot.session_id,
    problem_id: snapshot.problem_id,
    generation: snapshot.generation,
    status: snapshot.status,
    timeout_at: snapshot.timeout_at,
    cleanup_pending: snapshot.cleanup_pending,
    terminal_reason: snapshot.terminal_reason,
    event_sequence: cursor.event_sequence,
  }
}

function eventVerifyResult(event: LifecycleEventFrame): { changed: boolean; result: { success: boolean; log: string } | null } {
	if (event.event_type !== 'verify_finished') return { changed: false, result: null }
	if (event.payload.infrastructure_error === true) return { changed: true, result: null }
	if (typeof event.payload.success !== 'boolean') return { changed: false, result: null }
	return {
		changed: true,
		result: {
			success: event.payload.success,
			log: typeof event.payload.log === 'string' ? event.payload.log : '',
		},
	}
}

function applyLifecycleEvent(session: Session, event: LifecycleEventFrame): Session {
  let status = session.status
  let cleanupPending = session.cleanup_pending
  switch (event.event_type) {
    case 'allocation_reserved':
    case 'reset_requested':
      status = 'queued'
      break
    case 'vm_created':
    case 'booting':
      status = 'booting'
      break
    case 'setup_running':
      status = 'setting_up'
      break
    case 'ready':
      status = 'ready'
      cleanupPending = false
      break
    case 'verify_started':
      status = 'verifying'
      break
    case 'verify_finished':
      status = event.payload.success === true ? 'completed' : 'ready'
      break
    case 'failed':
      status = 'failed'
      cleanupPending = false
      break
    case 'cleanup_required':
      status = status === 'destroying' ? 'destroying' : 'failed'
      cleanupPending = true
      break
    case 'timed_out':
    case 'provider_lost':
      status = event.event_type
      cleanupPending = false
      break
    case 'destroying':
    case 'destroyed':
      status = event.event_type
      if (event.event_type === 'destroyed') cleanupPending = false
      break
  }
  if (typeof event.payload.cleanup_pending === 'boolean') cleanupPending = event.payload.cleanup_pending
  return {
    ...session,
    status,
    cleanup_pending: cleanupPending,
    terminal_reason: session.terminal_reason ||
      (['failed', 'timed_out', 'provider_lost', 'destroying', 'destroyed'].includes(event.event_type)
        ? event.reason_code || null
        : null),
    event_sequence: event.event_sequence,
  }
}

function mergeSessionSnapshot(current: Session, incoming: Session): Session {
  if (current.session_id !== incoming.session_id || current.problem_id !== incoming.problem_id ||
    current.generation !== incoming.generation) {
    return incoming
  }
  const currentRank = BOOT_STATUS_RANK[current.status]
  const incomingRank = BOOT_STATUS_RANK[incoming.status]
  if (currentRank !== undefined && incomingRank !== undefined && currentRank > incomingRank) {
    return { ...incoming, status: current.status }
  }
  return incoming
}

function mergeStaleSessionMetadata(current: Session, incoming: Session): Session {
  return {
    ...current,
    request_id: incoming.request_id ?? current.request_id,
  }
}

function retainEndIntent(intent: EndIntent | null, session: Session): EndIntent | null {
  return intent?.problemId === session.problem_id && intent.sessionId === session.session_id &&
    intent.generation === session.generation ? intent : null
}

export const useSessionStore = create<SessionState>((set, get) => ({
  session: null,
  stage: '',
  stageMessage: '',
  crashed: false,
  verifyResult: null,
  wsConnected: false,
  lifecycleCursor: null,
  lifecycleConnected: false,
  lifecycleResyncing: false,
  lifecycleAuthoritative: false,
  authorityEpoch: 0,
  authoritativeNull: false,
  endIntent: null,
  notice: null,
  setSession: (session) => set((state) => {
    if (session && state.session?.session_id === session.session_id && session.generation < state.session.generation) {
      return state
    }
    if (session && state.session?.session_id === session.session_id &&
      state.session.generation === session.generation && state.session.problem_id !== session.problem_id) {
      return state
    }
    const sameSessionScope = !!session && state.session?.session_id === session.session_id &&
      state.session.problem_id === session.problem_id && state.session.generation === session.generation
    const eventSequence = session?.event_sequence
    const incomingCursor = session && isPositiveInteger(eventSequence)
      ? { session_id: session.session_id, generation: session.generation, event_sequence: eventSequence }
      : null
    const sameCursorScope = !!sameSessionScope && state.lifecycleCursor?.session_id === session.session_id &&
      state.lifecycleCursor.generation === session.generation
    const staleAgainstCursor = !!session && !!state.session && !!sameCursorScope &&
      (!incomingCursor || state.lifecycleCursor!.event_sequence > incomingCursor.event_sequence)
    const nextSession = session && state.session
      ? staleAgainstCursor
        ? mergeStaleSessionMetadata(state.session, session)
        : mergeSessionSnapshot(state.session, session)
      : session
    const nextCursor = staleAgainstCursor
      ? state.lifecycleCursor
      : incomingCursor ?? (sameSessionScope ? state.lifecycleCursor : null)
    return {
      session: nextSession,
      endIntent: nextSession ? retainEndIntent(state.endIntent, nextSession) : state.endIntent,
      lifecycleCursor: nextCursor,
      lifecycleAuthoritative: sameSessionScope ? state.lifecycleAuthoritative : false,
      authorityEpoch: state.authorityEpoch + 1,
      authoritativeNull: session === null,
      crashed: false,
    }
  }),
  beginAuthorityRead: () => get().authorityEpoch,
  applyStartSnapshot: (snapshot, expectedAuthorityEpoch) => {
    let accepted = false
    set((state) => {
      const current = state.session
      const exactLifecycleAllocation = state.lifecycleAuthoritative && !!current &&
        current.session_id === snapshot.session_id && current.problem_id === snapshot.problem_id &&
        current.generation === snapshot.generation
      const unchangedEmptyAuthority = state.authorityEpoch === expectedAuthorityEpoch && current === null
      const endBlocksStart = state.endIntent?.problemId === snapshot.problem_id
      if (endBlocksStart || (!unchangedEmptyAuthority && !exactLifecycleAllocation)) return state

      accepted = true
      if (exactLifecycleAllocation && current) {
        // The lifecycle cursor is authoritative. The HTTP response can only
        // contribute request metadata; it must not move status/cursor back.
        return {
          session: mergeStaleSessionMetadata(current, snapshot),
          authorityEpoch: state.authorityEpoch + 1,
          authoritativeNull: false,
        }
      }

      const lifecycleCursor = isPositiveInteger(snapshot.event_sequence)
        ? {
            session_id: snapshot.session_id,
            generation: snapshot.generation,
            event_sequence: snapshot.event_sequence,
          }
        : null
      return {
        session: snapshot,
        endIntent: retainEndIntent(state.endIntent, snapshot),
        stage: lifecycleStage(snapshot.status),
        stageMessage: '',
        crashed: false,
        verifyResult: null,
        lifecycleCursor,
        lifecycleResyncing: false,
        lifecycleAuthoritative: false,
        authorityEpoch: state.authorityEpoch + 1,
        authoritativeNull: false,
      }
    })
    return accepted
  },
  applyCurrentHTTPSnapshot: (value, expectedEpoch) => {
    const snapshot = parseSessionSnapshot(value)
    if (!snapshot) return false
    let accepted = false
    set((state) => {
      if (state.authorityEpoch !== expectedEpoch) return state
      accepted = true
      return {
        session: snapshot,
        endIntent: retainEndIntent(state.endIntent, snapshot),
        stage: lifecycleStage(snapshot.status),
        stageMessage: '',
        crashed: false,
        verifyResult: null,
        lifecycleCursor: {
          session_id: snapshot.session_id,
          generation: snapshot.generation,
          event_sequence: snapshot.event_sequence,
        },
        lifecycleResyncing: false,
        lifecycleAuthoritative: false,
        authorityEpoch: state.authorityEpoch + 1,
        authoritativeNull: false,
      }
    })
    return accepted
  },
  applyCurrentHTTPNotFound: (expectedEpoch) => {
    let accepted = false
    set((state) => {
      if (state.authorityEpoch !== expectedEpoch) return state
      accepted = true
      return {
        session: null,
        stage: '',
        stageMessage: '',
        crashed: false,
        verifyResult: null,
        wsConnected: false,
        lifecycleCursor: null,
        lifecycleResyncing: false,
        lifecycleAuthoritative: false,
        authorityEpoch: state.authorityEpoch + 1,
        authoritativeNull: true,
      }
    })
    return accepted
  },
  applyResetSnapshot: (snapshot, sourceSessionId, sourceGeneration, expectedAuthorityEpoch) => {
    let accepted = false
    set((state) => {
      const current = state.session
      const targetGeneration = sourceGeneration + 1
      if (snapshot.session_id !== sourceSessionId || snapshot.generation !== targetGeneration) return state
      const endIntent = state.endIntent
      if (endIntent?.problemId === snapshot.problem_id && endIntent.sessionId === sourceSessionId &&
        (endIntent.generation === sourceGeneration || endIntent.generation === targetGeneration)) return state
      if (!current || current.session_id !== sourceSessionId || current.problem_id !== snapshot.problem_id) return state

      const exactSourceAtRequest = state.authorityEpoch === expectedAuthorityEpoch &&
        current.generation === sourceGeneration
      const exactLifecycleTarget = state.lifecycleAuthoritative && current.generation === targetGeneration
      if (exactSourceAtRequest) {
        accepted = true
        const lifecycleCursor = isPositiveInteger(snapshot.event_sequence)
          ? {
              session_id: snapshot.session_id,
              generation: snapshot.generation,
              event_sequence: snapshot.event_sequence,
            }
          : null
        return {
          session: snapshot,
          endIntent: retainEndIntent(state.endIntent, snapshot),
          lifecycleCursor,
          lifecycleResyncing: false,
          lifecycleAuthoritative: false,
          authorityEpoch: state.authorityEpoch + 1,
          authoritativeNull: false,
          crashed: false,
          verifyResult: null,
          stage: 'resetting',
          stageMessage: '환경을 다시 시작하는 중…',
        }
      }
      if (exactLifecycleTarget) {
        accepted = true
        return {
          // Preserve every lifecycle-owned field and cursor. Only the HTTP
          // request id is useful after lifecycle has already advanced.
          session: mergeStaleSessionMetadata(current, snapshot),
          authorityEpoch: state.authorityEpoch + 1,
          authoritativeNull: false,
        }
      }
      return state
    })
    return accepted
  },
  beginEndIntent: (intent, allowMissing = false) => {
    let accepted = false
    set((state) => {
      const current = state.session
      const exact = current?.problem_id === intent.problemId && current.session_id === intent.sessionId &&
        current.generation === intent.generation
      if (!exact && !(allowMissing && current === null)) return state
      accepted = true
      if (state.endIntent?.problemId === intent.problemId && state.endIntent.sessionId === intent.sessionId &&
        state.endIntent.generation === intent.generation) return state
      return {
        endIntent: intent,
        authorityEpoch: state.authorityEpoch + 1,
        wsConnected: false,
      }
    })
    return accepted
  },
  clearEndIntent: (sessionId, generation) => {
    let cleared = false
    set((state) => {
      if (!state.endIntent || state.endIntent.sessionId !== sessionId ||
        state.endIntent.generation !== generation) return state
      cleared = true
      return { endIntent: null, authorityEpoch: state.authorityEpoch + 1 }
    })
    return cleared
  },
  applyEndPending: (sessionId, generation) => {
    let accepted = false
    set((state) => {
      const current = state.session
      if (!current || current.session_id !== sessionId || current.generation !== generation) return state
      accepted = true
      return {
        session: { ...current, status: 'destroying', cleanup_pending: true },
        stage: 'destroying',
        stageMessage: '실행 환경을 안전하게 정리하는 중…',
        crashed: false,
        verifyResult: null,
        wsConnected: false,
        authorityEpoch: state.authorityEpoch + 1,
        authoritativeNull: false,
      }
    })
    return accepted
  },
  setStage: (stage, message) => set({ stage, stageMessage: message }),
  setCrashed: (crashed) => set({ crashed }),
  setVerifyResult: (result) => set({ verifyResult: result }),
  setWsConnected: (connected) => set({ wsConnected: connected }),
  setLifecycleConnected: (connected) => set({ lifecycleConnected: connected }),
  restoreLifecycleCursor: (cursor) => {
    let restored = false
    set((state) => {
      const session = state.session
      if (!session || session.session_id !== cursor.session_id || session.generation !== cursor.generation) return state
      const current = state.lifecycleCursor
      if (current && current.session_id === cursor.session_id && current.generation === cursor.generation &&
        current.event_sequence >= cursor.event_sequence) return state
      restored = true
      return { lifecycleCursor: cursor }
    })
    return restored
  },
  prepareLifecycleResync: () => set({
    lifecycleCursor: null,
    lifecycleResyncing: true,
    lifecycleAuthoritative: false,
  }),
  setNotice: (notice) => set({ notice }),
  handleLifecycleMessage: (value, _expectedProblemId, initialSnapshotAuthorityEpoch) => {
    const msg = classifyLifecycleMessage(value)
    if (!msg) {
      set({ lifecycleResyncing: true })
      return 'resync'
    }
    if (msg.type === 'lifecycle_resync_required') {
      set({ lifecycleResyncing: true })
      return 'resync'
    }
    if (msg.type === 'lifecycle_snapshot') {
      let result: LifecycleApplyResult = 'ignored'
      set((state) => {
        if (initialSnapshotAuthorityEpoch !== undefined &&
          state.authorityEpoch !== initialSnapshotAuthorityEpoch) {
          const exactCurrentAllocation = msg.session !== null && state.session !== null &&
            state.session.session_id === msg.session.session_id &&
            state.session.problem_id === msg.session.problem_id &&
            state.session.generation === msg.session.generation
          // Replay events can legitimately advance the epoch before their
          // trailing bootstrap snapshot arrives. Only that exact allocation
          // may still merge; every other stale initial snapshot must resync.
          if (!exactCurrentAllocation) {
            result = 'resync'
            return {
              lifecycleResyncing: true,
              lifecycleAuthoritative: false,
            }
          }
        }
        if (msg.session === null || msg.cursor === null) {
          result = 'applied'
          return {
            session: null,
            stage: '',
            stageMessage: '',
            crashed: false,
            verifyResult: null,
            lifecycleCursor: null,
            lifecycleResyncing: false,
            lifecycleAuthoritative: true,
            authorityEpoch: state.authorityEpoch + 1,
            authoritativeNull: true,
          }
        }
        const current = state.session
        if (current?.session_id === msg.session.session_id && current.generation > msg.session.generation) return state
        const cursor = state.lifecycleCursor
        if (cursor?.session_id === msg.cursor.session_id && cursor.generation === msg.cursor.generation &&
          cursor.event_sequence >= msg.cursor.event_sequence) return state

        const generationChanged = !current || current.session_id !== msg.session.session_id ||
          current.generation !== msg.session.generation
        result = 'applied'
        return {
          session: sessionFromLifecycle(msg.session, msg.cursor),
          endIntent: retainEndIntent(state.endIntent, sessionFromLifecycle(msg.session, msg.cursor)),
          stage: lifecycleStage(msg.session.status),
          stageMessage: '',
          crashed: false,
          verifyResult: msg.session.latest_verify_result ?? (generationChanged ? null : state.verifyResult),
          lifecycleCursor: msg.cursor,
          lifecycleResyncing: false,
          lifecycleAuthoritative: true,
          authorityEpoch: state.authorityEpoch + 1,
          authoritativeNull: false,
          notice: msg.session.status === 'provider_lost'
            ? '실행 환경이 예기치 않게 종료되어 세션을 정리하고 있습니다.'
            : state.notice,
        }
      })
      return result
    }

    let result: LifecycleApplyResult = 'ignored'
    set((state) => {
      const current = state.session
      const cursor = state.lifecycleCursor
      if (!current || !cursor) {
        result = 'resync'
        return { lifecycleResyncing: true }
      }
      if (msg.session_id !== current.session_id || msg.generation !== current.generation ||
        msg.session_id !== cursor.session_id || msg.generation !== cursor.generation) {
        if (msg.session_id === current.session_id && msg.generation < current.generation) return state
        result = 'resync'
        return { lifecycleResyncing: true }
      }
      if (msg.event_sequence <= cursor.event_sequence) return state
      if (msg.event_sequence !== cursor.event_sequence + 1) {
        result = 'resync'
        return { lifecycleResyncing: true }
      }
      const session = applyLifecycleEvent(current, msg)
		const verifyUpdate = eventVerifyResult(msg)
      result = 'applied'
      return {
        session,
        lifecycleCursor: {
          session_id: msg.session_id,
          generation: msg.generation,
          event_sequence: msg.event_sequence,
        },
        lifecycleResyncing: false,
        lifecycleAuthoritative: true,
        authorityEpoch: state.authorityEpoch + 1,
        authoritativeNull: false,
        stage: lifecycleStage(session.status),
        stageMessage: msg.message,
        crashed: false,
			verifyResult: verifyUpdate.changed ? verifyUpdate.result : state.verifyResult,
        notice: msg.event_type === 'provider_lost'
          ? '실행 환경이 예기치 않게 종료되어 세션을 정리하고 있습니다.'
          : state.notice,
      }
    })
    return result
  },
  handleWSMessage: (msg) => {
    const isCurrentEvent = (session: Session | null) =>
      !!session && msg.session_id === session.session_id && msg.generation === session.generation

    switch (msg.type) {
      case 'stage': {
        const stage = msg.stage || ''
        const nextStatus = STATUS_BY_STAGE[stage]
        set((state) => {
          if (!isCurrentEvent(state.session)) return state
          return {
            stage,
            stageMessage: msg.message || '',
            session:
              nextStatus && state.session && state.session.status !== nextStatus
                ? { ...state.session, status: nextStatus }
                : state.session,
          }
        })
        break
      }
      case 'verify_result':
        set((state) => isCurrentEvent(state.session)
          ? { verifyResult: { success: msg.success || false, log: msg.log || '' } }
          : state)
        break
      case 'timeout_warning':
        // The page derives its countdown from timeout_at. Identity validation
        // still prevents a delayed warning from being associated with a new
        // generation, even though no store mutation is needed here.
        break
      case 'session_ended':
        if (msg.reason === 'server_restart') {
          // Server restart is global and intentionally has no session identity.
          set((state) => ({
            session: null,
            stage: '',
            stageMessage: '',
            crashed: false,
            authorityEpoch: state.authorityEpoch + 1,
            authoritativeNull: true,
            notice: '서버가 재시작되어 진행 중이던 세션이 종료되었습니다.',
          }))
          break
        }
        if (msg.reason === 'reset') {
          set((state) => {
            const current = state.session
            if (!current || msg.session_id !== current.session_id || msg.generation !== current.generation + 1) {
              return state
            }
            return {
              crashed: false,
              stage: 'resetting',
              stageMessage: '환경을 다시 시작하는 중…',
              session: { ...current, generation: msg.generation, status: 'booting' },
              authorityEpoch: state.authorityEpoch + 1,
              authoritativeNull: false,
            }
          })
          break
        }
        if (!isCurrentEvent(useSessionStore.getState().session)) break
        if (msg.reason === 'container_crashed') {
          set({
            crashed: true,
            stage: 'container_crashed',
            stageMessage: '컨테이너가 중단되었습니다. 리셋하여 다시 시작하세요.',
          })
        } else if (msg.reason === 'provider_lost') {
          set((state) => ({
            session: null,
            stage: '',
            stageMessage: '',
            crashed: false,
            authorityEpoch: state.authorityEpoch + 1,
            authoritativeNull: true,
            notice: '실행 환경이 예기치 않게 종료되어 세션을 정리했습니다.',
          }))
        } else {
          // completed, timeout, and any unknown terminal reason.
          set((state) => ({
            session: null,
            stage: '',
            stageMessage: '',
            crashed: false,
            authorityEpoch: state.authorityEpoch + 1,
            authoritativeNull: true,
          }))
        }
        break
    }
  },
  clear: () => set((state) => ({
    session: null,
    stage: '',
    stageMessage: '',
    crashed: false,
    verifyResult: null,
    wsConnected: false,
    lifecycleCursor: null,
    lifecycleConnected: false,
    lifecycleResyncing: false,
    lifecycleAuthoritative: false,
    authorityEpoch: state.authorityEpoch + 1,
    authoritativeNull: false,
    endIntent: null,
  })),
}))
