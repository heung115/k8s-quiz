import { describe, it, expect, beforeEach } from 'vitest'
import { useSessionStore } from './session'
import { LIFECYCLE_SCHEMA } from '../types'
import type { LifecycleEventFrame, LifecycleSnapshotFrame, Session, WSMessage } from '../types'

const baseSession: Session = {
  operation_id: 'operation-1',
  session_id: 's1',
  problem_id: 'p1',
  generation: 1,
  status: 'creating',
  timeout_at: '2030-01-01T00:00:00Z',
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 1,
}

function reset(session: Session | null = { ...baseSession }) {
  useSessionStore.setState({
    session,
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
  })
}

const send = (msg: WSMessage) => useSessionStore.getState().handleWSMessage({
  session_id: 's1',
  generation: 1,
  ...msg,
})
const status = () => useSessionStore.getState().session?.status

function lifecycleSnapshot(
  generation: number,
  eventSequence: number,
  status: 'queued' | 'booting' | 'setting_up' | 'ready' | 'failed' = 'booting',
  overrides: Partial<NonNullable<LifecycleSnapshotFrame['session']>> = {},
): LifecycleSnapshotFrame {
  return {
    type: 'lifecycle_snapshot',
    schema: LIFECYCLE_SCHEMA,
    session: {
      session_id: 's1',
      problem_id: 'p1',
      generation,
      operation_id: `operation-${generation}`,
      status,
      timeout_at: '2030-01-01T00:00:00Z',
      cleanup_pending: false,
      terminal_reason: null,
      ...overrides,
    },
    cursor: { session_id: 's1', generation, event_sequence: eventSequence },
  }
}

function lifecycleEvent(
  generation: number,
  eventSequence: number,
  eventType: string,
  payload: Record<string, unknown> = {},
): LifecycleEventFrame {
  return {
    type: 'lifecycle_event',
    schema: LIFECYCLE_SCHEMA,
    session_id: 's1',
    generation,
    event_sequence: eventSequence,
    event_type: eventType,
    reason_code: 'test',
    message: eventType,
    occurred_at: '2030-01-01T00:00:00Z',
    payload,
  }
}

describe('session store — stage → status mapping', () => {
  beforeEach(() => reset())

  it('maps container_created → booting', () => {
    send({ type: 'stage', stage: 'container_created', message: 'Creating container...' })
    expect(status()).toBe('booting')
  })

  it('maps k3s_booting → booting', () => {
    send({ type: 'stage', stage: 'k3s_booting', message: 'Waiting for k3s to boot...' })
    expect(status()).toBe('booting')
  })

  it('maps setup_running → setting_up', () => {
    send({ type: 'stage', stage: 'setup_running', message: 'Setting up problem environment...' })
    expect(status()).toBe('setting_up')
  })

  it('maps ready → ready', () => {
    send({ type: 'stage', stage: 'ready', message: 'Environment ready. Good luck!' })
    expect(status()).toBe('ready')
  })

  it('walks the full boot sequence to ready', () => {
    send({ type: 'stage', stage: 'container_created' })
    send({ type: 'stage', stage: 'k3s_booting' })
    send({ type: 'stage', stage: 'setup_running' })
    send({ type: 'stage', stage: 'ready' })
    expect(status()).toBe('ready')
  })

  it('leaves status untouched for an unmapped stage (container_crashed)', () => {
    send({ type: 'stage', stage: 'ready' })
    expect(status()).toBe('ready')
    send({ type: 'stage', stage: 'container_crashed', message: 'stopped unexpectedly' })
    expect(status()).toBe('ready') // unchanged
    expect(useSessionStore.getState().stage).toBe('container_crashed')
  })

  it('ignores stages when there is no active session', () => {
    reset(null)
    send({ type: 'stage', stage: 'ready' })
    expect(useSessionStore.getState().session).toBeNull()
  })
})

describe('session store — session_ended reasons', () => {
  beforeEach(() => reset())

  it('reset → keeps the session and sets status booting', () => {
    send({ type: 'stage', stage: 'ready' })
    send({ type: 'session_ended', reason: 'reset', generation: 2 })
    const s = useSessionStore.getState().session
    expect(s).not.toBeNull()
    expect(s?.status).toBe('booting')
    expect(s?.generation).toBe(2)
    expect(useSessionStore.getState().crashed).toBe(false)
  })

  it('server_restart → clears the session and sets a notice', () => {
    send({ type: 'session_ended', reason: 'server_restart', session_id: undefined, generation: undefined })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().notice).toBeTruthy()
  })

  it('timeout (default path) → clears the session, leaves notice unset', () => {
    send({ type: 'session_ended', reason: 'timeout' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().notice).toBeNull()
  })

  it('container_crashed → flags crashed and keeps the session', () => {
    send({ type: 'session_ended', reason: 'container_crashed' })
    expect(useSessionStore.getState().crashed).toBe(true)
    expect(useSessionStore.getState().session).not.toBeNull()
  })

  it('provider_lost → clears the removed server session and sets a notice', () => {
    send({ type: 'session_ended', reason: 'provider_lost' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().crashed).toBe(false)
    expect(useSessionStore.getState().notice).toBeTruthy()
  })

  it('completed → clears the current session', () => {
    send({ type: 'session_ended', reason: 'completed' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().crashed).toBe(false)
  })

  it('environment_failed → clears the current session', () => {
    send({ type: 'session_ended', reason: 'environment_failed' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().crashed).toBe(false)
  })

  it('ignores delayed terminal events from an older generation', () => {
    reset({ ...baseSession, generation: 2, status: 'ready' })
    send({ type: 'session_ended', reason: 'provider_lost', generation: 1 })
    expect(useSessionStore.getState().session?.generation).toBe(2)
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().notice).toBeNull()
  })

  it('ignores delayed stages from an older generation', () => {
    reset({ ...baseSession, generation: 2, status: 'ready' })
    send({ type: 'stage', stage: 'k3s_booting', generation: 1 })
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().stage).toBe('')
  })

  it('rejects reset events that skip or repeat generations', () => {
    send({ type: 'session_ended', reason: 'reset', generation: 3 })
    expect(useSessionStore.getState().session?.generation).toBe(1)
    send({ type: 'session_ended', reason: 'reset', generation: 2 })
    expect(useSessionStore.getState().session?.generation).toBe(2)
    send({ type: 'session_ended', reason: 'reset', generation: 2 })
    expect(useSessionStore.getState().session?.generation).toBe(2)
  })
})

describe('session store — reset snapshot ordering', () => {
  beforeEach(() => reset())

  const replacement: Session = {
    ...baseSession,
    operation_id: 'reset-operation-2',
    generation: 2,
    status: 'booting',
  }

  it('applies exactly source generation to next generation', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    const applied = useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, epoch)
    expect(applied).toBe(true)
    expect(useSessionStore.getState().session).toEqual(replacement)
    expect(useSessionStore.getState().stage).toBe('resetting')
  })

  it('preserves a same-generation status already advanced by WebSocket', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(2, 5, 'ready'))
    const epoch = 0
    const applied = useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, epoch)
    expect(applied).toBe(true)
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().stage).toBe('ready')
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(5)
    expect(useSessionStore.getState().lifecycleAuthoritative).toBe(true)
  })

  it('rejects delayed and skipped reset snapshots', () => {
    reset({ ...baseSession, generation: 3, status: 'ready' })
    expect(useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, 0)).toBe(false)
    expect(useSessionStore.getState().session?.generation).toBe(3)

    reset()
    expect(useSessionStore.getState().applyResetSnapshot({ ...replacement, generation: 3 }, 's1', 1, 0)).toBe(false)
    expect(useSessionStore.getState().session?.generation).toBe(1)
  })

  it('does not resurrect an exact reset response after authoritative null', () => {
    reset(null)
    expect(useSessionStore.getState().applyResetSnapshot({ ...replacement, event_sequence: 2 }, 's1', 1, 0)).toBe(false)
    expect(useSessionStore.getState().session).toBeNull()
  })

  it('rejects a reset response when the source generation is being ended', () => {
    expect(useSessionStore.getState().beginEndIntent({
      problemId: 'p1', sessionId: 's1', generation: 1,
    })).toBe(true)
    expect(useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, 0)).toBe(false)
    expect(useSessionStore.getState().session).toMatchObject({ generation: 1 })
    expect(useSessionStore.getState().endIntent).toMatchObject({ generation: 1 })
  })

  it('rejects a late reset response when the target generation is being ended', () => {
    reset({ ...replacement, status: 'destroying', cleanup_pending: true })
    expect(useSessionStore.getState().beginEndIntent({
      problemId: 'p1', sessionId: 's1', generation: 2,
    })).toBe(true)
    expect(useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, 0)).toBe(false)
    expect(useSessionStore.getState().session).toMatchObject({
      generation: 2, status: 'destroying', cleanup_pending: true,
    })
    expect(useSessionStore.getState().endIntent).toMatchObject({ generation: 2 })
  })

  it('does not let a lower ordinary snapshot replace the same logical session', () => {
    reset({ ...baseSession, generation: 3, status: 'ready' })
    useSessionStore.getState().setSession({ ...baseSession, generation: 2, status: 'booting' })
    expect(useSessionStore.getState().session?.generation).toBe(3)
    expect(useSessionStore.getState().session?.status).toBe('ready')
  })

  it('rejects a late reset after authority moves to another allocation', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    useSessionStore.getState().handleLifecycleMessage({
      ...lifecycleSnapshot(1, 2, 'ready', { session_id: 's2' }),
      cursor: { session_id: 's2', generation: 1, event_sequence: 2 },
    })
    expect(useSessionStore.getState().applyResetSnapshot(replacement, 's1', 1, epoch)).toBe(false)
    expect(useSessionStore.getState().session?.session_id).toBe('s2')
  })
})

describe('session store — start snapshot ordering', () => {
  beforeEach(() => reset(null))

  const started: Session = { ...baseSession, status: 'creating', request_id: 'start:key' }

  it('installs a start response only while empty authority is unchanged', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    expect(useSessionStore.getState().applyStartSnapshot(started, epoch)).toBe(true)
    expect(useSessionStore.getState().session).toEqual(started)
  })

  it('rejects a late start after lifecycle authoritative null', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    useSessionStore.getState().handleLifecycleMessage({
      type: 'lifecycle_snapshot', schema: LIFECYCLE_SCHEMA, session: null, cursor: null,
    })
    expect(useSessionStore.getState().applyStartSnapshot(started, epoch)).toBe(false)
    expect(useSessionStore.getState().session).toBeNull()
  })

  it('preserves a started session and resyncs on a late initial lifecycle null', () => {
    const socketEpoch = useSessionStore.getState().beginAuthorityRead()
    expect(useSessionStore.getState().applyStartSnapshot(started, socketEpoch)).toBe(true)

    expect(useSessionStore.getState().handleLifecycleMessage({
      type: 'lifecycle_snapshot', schema: LIFECYCLE_SCHEMA, session: null, cursor: null,
    }, 'p1', socketEpoch)).toBe('resync')

    expect(useSessionStore.getState().session).toEqual(started)
    expect(useSessionStore.getState().lifecycleResyncing).toBe(true)
    expect(useSessionStore.getState().lifecycleAuthoritative).toBe(false)
  })

  it('preserves a newer allocation and resyncs on a stale non-null initial snapshot', () => {
    const socketEpoch = useSessionStore.getState().beginAuthorityRead()
    const newer = { ...started, session_id: 's2', operation_id: 'operation-newer' }
    expect(useSessionStore.getState().applyStartSnapshot(newer, socketEpoch)).toBe(true)

    expect(useSessionStore.getState().handleLifecycleMessage(
      lifecycleSnapshot(1, 2, 'ready'), 'p1', socketEpoch,
    )).toBe('resync')

    expect(useSessionStore.getState().session).toEqual(newer)
    expect(useSessionStore.getState().lifecycleResyncing).toBe(true)
  })

  it('accepts an initial snapshot for the exact allocation advanced by replay events', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 1, 'booting'))
    const socketEpoch = useSessionStore.getState().beginAuthorityRead()
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 2, 'setup_running'))).toBe('applied')

    expect(useSessionStore.getState().handleLifecycleMessage(
      lifecycleSnapshot(1, 3, 'ready'), 'p1', socketEpoch,
    )).toBe('applied')
    expect(useSessionStore.getState().session).toMatchObject({ session_id: 's1', generation: 1, status: 'ready' })
  })

  it('merges metadata without regressing an exact lifecycle allocation', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 7, 'ready'))
    expect(useSessionStore.getState().applyStartSnapshot(started, epoch)).toBe(true)
    expect(useSessionStore.getState().session).toMatchObject({
      request_id: 'start:key', status: 'ready', event_sequence: 7,
    })
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(7)
  })

  it('rejects another lifecycle allocation and a conflicting end intent', () => {
    const epoch = useSessionStore.getState().authorityEpoch
    const replacementSnapshot = lifecycleSnapshot(1, 2, 'ready', { session_id: 's2' })
    replacementSnapshot.cursor = { session_id: 's2', generation: 1, event_sequence: 2 }
    useSessionStore.getState().handleLifecycleMessage(replacementSnapshot)
    expect(useSessionStore.getState().applyStartSnapshot(started, epoch)).toBe(false)

    reset(null)
    useSessionStore.setState({ endIntent: { problemId: 'p1', sessionId: 'old', generation: 1 } })
    expect(useSessionStore.getState().applyStartSnapshot(started, 0)).toBe(false)
  })
})

describe('session store — durable end state', () => {
  beforeEach(() => reset({ ...baseSession, status: 'ready', cleanup_pending: false }))

  it('marks only the exact allocation as destroying and cleanup pending', () => {
    expect(useSessionStore.getState().applyEndPending('s1', 1)).toBe(true)
    expect(useSessionStore.getState().session).toMatchObject({
      session_id: 's1', generation: 1, status: 'destroying', cleanup_pending: true,
    })
    expect(useSessionStore.getState().stage).toBe('destroying')
    expect(useSessionStore.getState().wsConnected).toBe(false)
  })

  it('ignores an old session or generation end response', () => {
    expect(useSessionStore.getState().applyEndPending('other', 1)).toBe(false)
    expect(useSessionStore.getState().applyEndPending('s1', 2)).toBe(false)
    expect(useSessionStore.getState().session).toMatchObject({ status: 'ready', cleanup_pending: false })
  })

  it('keeps cleanup_required pending and preserves an existing destroying state', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 4, 'ready'))
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 5, 'cleanup_required'))).toBe('applied')
    expect(useSessionStore.getState().session).toMatchObject({ status: 'failed', cleanup_pending: true })

    useSessionStore.getState().applyEndPending('s1', 1)
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 6, 'cleanup_required'))).toBe('applied')
    expect(useSessionStore.getState().session).toMatchObject({ status: 'destroying', cleanup_pending: true })
  })
})

describe('session store — lifecycle verify results', () => {
	beforeEach(() => reset())

	it('restores the latest grade from an authoritative snapshot', () => {
		const snapshot = lifecycleSnapshot(1, 4, 'ready', {
			latest_verify_result: { success: false, log: 'still broken' },
		})
		expect(useSessionStore.getState().handleLifecycleMessage(snapshot, 'p1')).toBe('applied')
		expect(useSessionStore.getState().verifyResult).toEqual({ success: false, log: 'still broken' })
	})

	it('clears a stale grade when verify infrastructure fails', () => {
		useSessionStore.setState({
			session: { ...baseSession, status: 'verifying', event_sequence: 3 },
			lifecycleCursor: { session_id: 's1', generation: 1, event_sequence: 3 },
			verifyResult: { success: true, log: 'old pass' },
		})
		const event = lifecycleEvent(1, 4, 'verify_finished', {
			infrastructure_error: true,
			code: 'controller_restarted',
		})
		expect(useSessionStore.getState().handleLifecycleMessage(event, 'p1')).toBe('applied')
		expect(useSessionStore.getState().session?.status).toBe('ready')
		expect(useSessionStore.getState().verifyResult).toBeNull()
	})
})

describe('session store — same-generation snapshot ordering', () => {
  beforeEach(() => reset())

  it('does not let an older HTTP watermark overwrite lifecycle-owned state', () => {
    reset({
      ...baseSession,
      operation_id: 'operation-1',
      status: 'destroying',
      cleanup_pending: true,
      terminal_reason: 'completed',
      event_sequence: 10,
    })
    useSessionStore.setState({
      lifecycleCursor: { session_id: 's1', generation: 1, event_sequence: 10 },
      lifecycleAuthoritative: true,
    })

    useSessionStore.getState().setSession({
      ...baseSession,
      operation_id: 'operation-1',
      request_id: 'late-http',
      status: 'ready',
      cleanup_pending: false,
      terminal_reason: null,
      event_sequence: 9,
    })

    expect(useSessionStore.getState().session).toMatchObject({
      request_id: 'late-http',
      status: 'destroying',
      cleanup_pending: true,
      terminal_reason: 'completed',
      event_sequence: 10,
    })
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(10)
    expect(useSessionStore.getState().lifecycleAuthoritative).toBe(true)
  })

  it('does not let a watermark-free HTTP snapshot overwrite lifecycle-owned state', () => {
    reset({ ...baseSession, status: 'verifying', cleanup_pending: true, event_sequence: 10 })
    useSessionStore.setState({
      lifecycleCursor: { session_id: 's1', generation: 1, event_sequence: 10 },
      lifecycleAuthoritative: true,
    })

    useSessionStore.getState().setSession({
      ...baseSession,
      request_id: 'legacy-http',
      status: 'ready',
      cleanup_pending: false,
    })

    expect(useSessionStore.getState().session).toMatchObject({
      request_id: 'legacy-http',
      status: 'verifying',
      cleanup_pending: true,
      event_sequence: 10,
    })
  })

  it('keeps ready after a delayed booting HTTP snapshot', () => {
    reset({ ...baseSession, status: 'ready' })
    useSessionStore.getState().setSession({ ...baseSession, status: 'booting', request_id: 'late-http' })
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().session?.request_id).toBe('late-http')
  })

  it('keeps setting_up after a delayed creating snapshot', () => {
    reset({ ...baseSession, status: 'setting_up' })
    useSessionStore.getState().setSession({ ...baseSession, status: 'creating' })
    expect(useSessionStore.getState().session?.status).toBe('setting_up')
  })

  it('accepts forward boot progress and a new generation', () => {
    reset({ ...baseSession, status: 'booting' })
    useSessionStore.getState().setSession({ ...baseSession, status: 'ready' })
    expect(useSessionStore.getState().session?.status).toBe('ready')
    useSessionStore.getState().setSession({ ...baseSession, generation: 2, status: 'creating' })
    expect(useSessionStore.getState().session?.generation).toBe(2)
    expect(useSessionStore.getState().session?.status).toBe('creating')
  })

  it('does not force non-boot transitions to be monotonic', () => {
    reset({ ...baseSession, status: 'verifying' })
    useSessionStore.getState().setSession({ ...baseSession, status: 'ready' })
    expect(useSessionStore.getState().session?.status).toBe('ready')
  })
})

describe('session store — authoritative lifecycle replay', () => {
  beforeEach(() => reset())

  it('applies each contiguous sequence once and ignores a duplicate', () => {
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 4))).toBe('applied')
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 5, 'setup_running'))).toBe('applied')
    expect(useSessionStore.getState().session?.status).toBe('setting_up')
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 5, 'failed'))).toBe('ignored')
    expect(useSessionStore.getState().session?.status).toBe('setting_up')
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 6, 'ready'))).toBe('applied')
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(6)
  })

  it('does not apply an event over a sequence gap and requests resync', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 6, 'ready'))
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 8, 'failed'))).toBe('resync')
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(6)
    expect(useSessionStore.getState().lifecycleResyncing).toBe(true)
  })

  it('rejects delayed events from a stale generation', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(2, 2, 'booting'))
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 3, 'failed'))).toBe('ignored')
    expect(useSessionStore.getState().session?.generation).toBe(2)
    expect(useSessionStore.getState().session?.status).toBe('booting')
    expect(useSessionStore.getState().lifecycleResyncing).toBe(false)
  })

  it('authoritative higher-generation snapshot replaces cursor and generation-local state', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 9, 'ready'))
    useSessionStore.setState({
      crashed: true,
      verifyResult: { success: false, log: 'old allocation result' },
      stage: 'container_crashed',
    })
    expect(useSessionStore.getState().handleLifecycleMessage(
      lifecycleSnapshot(2, 2, 'queued', { cleanup_pending: true }),
    )).toBe('applied')
    expect(useSessionStore.getState().session).toMatchObject({
      generation: 2,
      status: 'queued',
      cleanup_pending: true,
    })
    expect(useSessionStore.getState().lifecycleCursor).toEqual({
      session_id: 's1', generation: 2, event_sequence: 2,
    })
    expect(useSessionStore.getState().crashed).toBe(false)
    expect(useSessionStore.getState().verifyResult).toBeNull()
  })

  it('same-generation snapshot cannot regress below the applied cursor', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 5, 'setting_up'))
    useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 6, 'ready'))
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 5, 'booting'))).toBe('ignored')
    expect(useSessionStore.getState().session?.status).toBe('ready')
    expect(useSessionStore.getState().lifecycleCursor?.event_sequence).toBe(6)
  })

  it('ignores a conflicting same-watermark snapshot', () => {
    useSessionStore.getState().handleLifecycleMessage(
      lifecycleSnapshot(2, 2, 'queued', { cleanup_pending: true }),
    )
    expect(useSessionStore.getState().handleLifecycleMessage(
      lifecycleSnapshot(2, 2, 'booting', { cleanup_pending: false }),
    )).toBe('ignored')
    expect(useSessionStore.getState().session).toMatchObject({
      generation: 2,
      status: 'queued',
      cleanup_pending: true,
      event_sequence: 2,
    })
  })

  it('does not preserve a cursor when an ordinary snapshot changes session identity', () => {
    useSessionStore.getState().handleLifecycleMessage(lifecycleSnapshot(1, 5, 'ready'))
    useSessionStore.getState().setSession({
      ...baseSession,
      session_id: 's2',
      generation: 1,
      status: 'booting',
      event_sequence: 1,
    })
    expect(useSessionStore.getState().session?.session_id).toBe('s2')
    expect(useSessionStore.getState().lifecycleCursor).toEqual({
      session_id: 's2', generation: 1, event_sequence: 1,
    })
    expect(useSessionStore.getState().lifecycleAuthoritative).toBe(false)
  })
})

describe('session store — HTTP authority epoch', () => {
  beforeEach(() => reset(null))

  const currentSnapshot = (overrides: Partial<Session> = {}): Session => ({
    ...baseSession,
    status: 'ready',
    cleanup_pending: false,
    event_sequence: 4,
    ...overrides,
  })

  it('rejects a late GET snapshot after an authoritative lifecycle null', () => {
    const epoch = useSessionStore.getState().beginAuthorityRead()
    expect(useSessionStore.getState().handleLifecycleMessage({
      type: 'lifecycle_snapshot', schema: LIFECYCLE_SCHEMA, session: null, cursor: null,
    })).toBe('applied')
    expect(useSessionStore.getState().applyCurrentHTTPSnapshot(currentSnapshot(), epoch)).toBe(false)
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().authoritativeNull).toBe(true)
  })

  it('rejects a late old session and late 404 after a newer logical session', () => {
    const snapshotEpoch = useSessionStore.getState().beginAuthorityRead()
    const missingEpoch = useSessionStore.getState().beginAuthorityRead()
    const newer = lifecycleSnapshot(1, 2, 'ready', { session_id: 's2', problem_id: 'p2' })
    newer.cursor = { session_id: 's2', generation: 1, event_sequence: 2 }
    expect(useSessionStore.getState().handleLifecycleMessage(newer, 'p1')).toBe('applied')
    expect(useSessionStore.getState().applyCurrentHTTPSnapshot(currentSnapshot(), snapshotEpoch)).toBe(false)
    expect(useSessionStore.getState().applyCurrentHTTPNotFound(missingEpoch)).toBe(false)
    expect(useSessionStore.getState().session).toMatchObject({ session_id: 's2', problem_id: 'p2' })
  })

  it('rejects malformed current responses without changing authority', () => {
    const epoch = useSessionStore.getState().beginAuthorityRead()
    expect(useSessionStore.getState().applyCurrentHTTPSnapshot({ ...currentSnapshot(), cleanup_pending: undefined }, epoch)).toBe(false)
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().authorityEpoch).toBe(epoch)
  })

  it('applies an exact 404 only while its read token is current', () => {
    reset(currentSnapshot())
    const epoch = useSessionStore.getState().beginAuthorityRead()
    expect(useSessionStore.getState().applyCurrentHTTPNotFound(epoch)).toBe(true)
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().authoritativeNull).toBe(true)
  })
})

describe('session store — persistent end intent overlay', () => {
  beforeEach(() => reset({ ...baseSession, status: 'ready' }))

  it('retains the exact overlay across lifecycle state changes and null', () => {
    const intent = { problemId: 'p1', sessionId: 's1', generation: 1 }
    expect(useSessionStore.getState().beginEndIntent(intent)).toBe(true)
    useSessionStore.setState({
      lifecycleCursor: { session_id: 's1', generation: 1, event_sequence: 1 },
    })
    expect(useSessionStore.getState().handleLifecycleMessage(lifecycleEvent(1, 2, 'failed'))).toBe('applied')
    expect(useSessionStore.getState().endIntent).toEqual(intent)
    expect(useSessionStore.getState().handleLifecycleMessage({
      type: 'lifecycle_snapshot', schema: LIFECYCLE_SCHEMA, session: null, cursor: null,
    })).toBe('applied')
    expect(useSessionStore.getState().endIntent).toEqual(intent)
  })

  it('cannot attach an old end intent to a replacement session', () => {
    reset({ ...baseSession, session_id: 's2', generation: 1, status: 'ready' })
    expect(useSessionStore.getState().beginEndIntent({
      problemId: 'p1', sessionId: 's1', generation: 1,
    })).toBe(false)
    expect(useSessionStore.getState().endIntent).toBeNull()
  })
})
