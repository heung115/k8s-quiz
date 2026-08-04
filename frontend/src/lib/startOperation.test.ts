import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  clearAllPendingStarts,
  clearPendingStart,
  getOrCreatePendingStart,
  isValidStartResponse,
  loadPendingStart,
} from './startOperation'

describe('durable start operation state', () => {
  beforeEach(() => {
    vi.unstubAllGlobals()
    sessionStorage.clear()
    vi.restoreAllMocks()
  })

  it('creates once and reuses the same key for an ambiguous retry', () => {
    vi.stubGlobal('crypto', { randomUUID: () => '11111111-1111-4111-8111-111111111111' })
    const first = getOrCreatePendingStart('user-1', 'pod-crash')
    const retry = getOrCreatePendingStart('user-1', 'pod-crash')
    expect(first.key).toBe('start:pod-crash:11111111-1111-4111-8111-111111111111')
    expect(retry).toEqual(first)
  })

  it('scopes pending operations by user and problem', () => {
    const first = getOrCreatePendingStart('user-1', 'p1')
    const otherProblem = getOrCreatePendingStart('user-1', 'p2')
    const otherUser = getOrCreatePendingStart('user-2', 'p1')
    expect(otherProblem.key).not.toBe(first.key)
    expect(otherUser.key).not.toBe(first.key)
  })

  it('clears only the acknowledged exact key', () => {
    const pending = getOrCreatePendingStart('user-1', 'p1')
    clearPendingStart('user-1', 'p1', 'different-key')
    expect(loadPendingStart('user-1', 'p1')).toEqual(pending)
    clearPendingStart('user-1', 'p1', pending.key)
    expect(loadPendingStart('user-1', 'p1')).toBeNull()
  })

  it('purges every pending start on auth identity teardown', () => {
    getOrCreatePendingStart('user-1', 'p1')
    getOrCreatePendingStart('user-2', 'p2')
    clearAllPendingStarts()
    expect(loadPendingStart('user-1', 'p1')).toBeNull()
    expect(loadPendingStart('user-2', 'p2')).toBeNull()
  })

  it('accepts only a complete operation-bound session response', () => {
    const valid = {
      request_id: 'request-1', operation_id: 'op-1', session_id: 'session-1', problem_id: 'p1', generation: 1,
      status: 'booting', timeout_at: '2030-01-01T00:00:00Z', cleanup_pending: false,
      terminal_reason: null, event_sequence: 2,
    }
    expect(isValidStartResponse(valid, 'p1', 'request-1')).toBe(true)
    expect(isValidStartResponse({ ...valid, generation: 0 }, 'p1', 'request-1')).toBe(false)
    expect(isValidStartResponse({ ...valid, operation_id: '' }, 'p1', 'request-1')).toBe(false)
    expect(isValidStartResponse({ ...valid, request_id: 'other-request' }, 'p1', 'request-1')).toBe(false)
    expect(isValidStartResponse(valid, 'p2', 'request-1')).toBe(false)
    expect(isValidStartResponse({ ...valid, event_sequence: 0 }, 'p1', 'request-1')).toBe(false)
  })
})
