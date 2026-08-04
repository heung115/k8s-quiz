import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Session, User } from '../types'
import { getOrCreatePendingReset, loadPendingReset } from '../lib/resetOperation'
import { getOrCreatePendingStart, loadPendingStart } from '../lib/startOperation'
import { getOrCreatePendingEnd, loadPendingEnd } from '../lib/endOperation'
import { getOrCreatePendingVerify, loadPendingVerify } from '../lib/verifyOperation'
import { getOrCreatePendingChoice, loadPendingChoice } from '../lib/choiceOperation'
import { useSessionStore } from './session'
import { useAuthStore } from './auth'
import { APIResponseError, api } from '../api/client'

const user = { id: 'user-1' } as User
const session: Session = {
  operation_id: 'operation-1',
  session_id: 'session-1',
  problem_id: 'p1',
  generation: 1,
  status: 'ready',
  timeout_at: '2030-01-01T00:00:00Z',
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 1,
}

const verifyScope = {
  userId: user.id,
  problemId: session.problem_id,
  sessionId: session.session_id,
  generation: session.generation,
}

const choiceScope = { ...verifyScope, choiceId: 'choice-a' }

function seedRuntimeState() {
  useAuthStore.setState({ user, bootstrapped: true })
  useSessionStore.setState({ session, stage: 'ready', wsConnected: true })
  getOrCreatePendingStart(user.id, 'p1')
  getOrCreatePendingReset(user.id, 'p1', session.session_id, session.generation)
  getOrCreatePendingEnd({
    userId: user.id,
    problemId: session.problem_id,
    sessionId: session.session_id,
    generation: session.generation,
  })
  getOrCreatePendingVerify(verifyScope)
  getOrCreatePendingChoice(choiceScope)
}

function expectRuntimeStateCleared() {
  expect(useAuthStore.getState().user).toBeNull()
  expect(useSessionStore.getState().session).toBeNull()
  expect(useSessionStore.getState().stage).toBe('')
  expect(useSessionStore.getState().wsConnected).toBe(false)
  expect(loadPendingStart(user.id, 'p1')).toBeNull()
  expect(loadPendingReset(user.id, session.session_id, session.generation)).toBeNull()
  expect(loadPendingEnd({
    userId: user.id,
    problemId: session.problem_id,
    sessionId: session.session_id,
    generation: session.generation,
  })).toBeNull()
  expect(loadPendingVerify(verifyScope)).toBeNull()
  expect(loadPendingChoice(choiceScope)).toBeNull()
}

function expectRuntimeStatePreserved() {
  expect(useAuthStore.getState().user?.id).toBe(user.id)
  expect(useSessionStore.getState().session).toMatchObject({
    session_id: session.session_id,
    generation: session.generation,
  })
  expect(useSessionStore.getState().stage).toBe('ready')
  expect(loadPendingStart(user.id, 'p1')).not.toBeNull()
  expect(loadPendingReset(user.id, session.session_id, session.generation)).not.toBeNull()
  expect(loadPendingEnd({
    userId: user.id,
    problemId: session.problem_id,
    sessionId: session.session_id,
    generation: session.generation,
  })).not.toBeNull()
  expect(loadPendingVerify(verifyScope)).not.toBeNull()
  expect(loadPendingChoice(choiceScope)).not.toBeNull()
}

describe('auth teardown', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true })))
    useSessionStore.getState().clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('clearAuth removes the identity, session, connection state, and retry ledgers', () => {
    seedRuntimeState()
    useAuthStore.getState().clearAuth()
    expectRuntimeStateCleared()
  })

  it('logout clears local runtime state even though revocation is best effort', () => {
    seedRuntimeState()
    useAuthStore.getState().logout()
    expectRuntimeStateCleared()
  })

  it.each([
    ['transport failure', new TypeError('network down')],
    ['HTTP 503', new APIResponseError(503, 'temporarily unavailable')],
  ])('bootstrap preserves runtime state and ledgers after %s', async (_name, failure) => {
    seedRuntimeState()
    vi.spyOn(api, 'get').mockRejectedValueOnce(failure)

    await useAuthStore.getState().bootstrap()

    expect(useAuthStore.getState().bootstrapped).toBe(true)
    expectRuntimeStatePreserved()
  })
})
