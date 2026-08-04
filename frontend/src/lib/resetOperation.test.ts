import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  acknowledgePendingReset,
  clearAllPendingResets,
  clearPendingReset,
  clearPendingResetAfterLifecycle,
  getOrCreatePendingReset,
  isValidResetResponse,
  loadPendingReset,
  loadPendingResetForProblem,
} from './resetOperation'
import type { Session } from '../types'

describe('durable reset operation state', () => {
  beforeEach(() => {
    vi.unstubAllGlobals()
    sessionStorage.clear()
    vi.restoreAllMocks()
  })

  it('creates once and reuses the key for the exact source generation', () => {
    vi.stubGlobal('crypto', { randomUUID: () => '11111111-1111-4111-8111-111111111111' })
    const first = getOrCreatePendingReset('user-1', 'p1', 'session-1', 3)
    const retry = getOrCreatePendingReset('user-1', 'p1', 'session-1', 3)
    expect(first.key).toBe('reset:3:11111111-1111-4111-8111-111111111111')
    expect(retry).toEqual(first)
  })

  it('scopes reset keys by user, session, and source generation', () => {
    const first = getOrCreatePendingReset('user-1', 'p1', 'session-1', 1)
    expect(getOrCreatePendingReset('user-2', 'p1', 'session-1', 1).key).not.toBe(first.key)
    expect(getOrCreatePendingReset('user-1', 'p1', 'session-2', 1).key).not.toBe(first.key)
    expect(getOrCreatePendingReset('user-1', 'p1', 'session-1', 2).key).not.toBe(first.key)
  })

  it('finds the newest pending reset for reload recovery', () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(100)
    getOrCreatePendingReset('user-1', 'p1', 'session-1', 1)
    now.mockReturnValue(200)
    const latest = getOrCreatePendingReset('user-1', 'p1', 'session-1', 2)
    expect(loadPendingResetForProblem('user-1', 'p1', 300)).toEqual(latest)
    expect(loadPendingResetForProblem('user-1', 'p2', 300)).toBeNull()
  })

  it('clears only the exact acknowledged operation and expires stale records', () => {
    const pending = getOrCreatePendingReset('user-1', 'p1', 'session-1', 1)
    clearPendingReset({ ...pending, key: 'reset:1:different-operation' })
    expect(loadPendingReset('user-1', 'session-1', 1)).toEqual(pending)
    clearPendingReset(pending)
    expect(loadPendingReset('user-1', 'session-1', 1)).toBeNull()

    vi.spyOn(Date, 'now').mockReturnValue(1_000)
    getOrCreatePendingReset('user-1', 'p1', 'session-1', 2)
    expect(loadPendingReset('user-1', 'session-1', 2, 1_000 + 24 * 60 * 60 * 1000 + 1)).toBeNull()
  })

  it('purges every reset operation on auth identity teardown', () => {
    getOrCreatePendingReset('user-1', 'p1', 'session-1', 1)
    getOrCreatePendingReset('user-2', 'p2', 'session-2', 4)
    clearAllPendingResets()
    expect(loadPendingReset('user-1', 'session-1', 1)).toBeNull()
    expect(loadPendingReset('user-2', 'session-2', 4)).toBeNull()
  })

  it('accepts only the operation-bound next-generation snapshot', () => {
    const pending = getOrCreatePendingReset('user-1', 'p1', 'session-1', 3)
    const valid = {
      request_id: pending.key, operation_id: 'op-reset', session_id: 'session-1', problem_id: 'p1', generation: 4,
      status: 'booting', timeout_at: '2030-01-01T00:00:00Z', cleanup_pending: true,
      terminal_reason: null, event_sequence: 2,
    }
    expect(isValidResetResponse(valid, pending)).toBe(true)
    expect(isValidResetResponse({ ...valid, session_id: 'session-2' }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, problem_id: 'p2' }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, generation: 3 }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, generation: 5 }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, operation_id: '' }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, request_id: 'reset:3:different-request' }, pending)).toBe(false)
    expect(isValidResetResponse({ ...valid, event_sequence: 0 }, pending)).toBe(false)
  })

  it('keeps cleanup-pending state until the exact operation and generation converge', () => {
    const pending = getOrCreatePendingReset('user-1', 'p1', 'session-1', 3)
    const accepted: Session = {
      request_id: pending.key,
      operation_id: 'op-reset',
      session_id: 'session-1',
      problem_id: 'p1',
      generation: 4,
      status: 'queued',
      timeout_at: '2030-01-01T00:00:00Z',
      cleanup_pending: true,
      terminal_reason: null,
      event_sequence: 2,
    }
    const acceptedAt = pending.createdAt + 1
    expect(acknowledgePendingReset(pending, accepted, acceptedAt)).toMatchObject({
      operationId: 'op-reset', targetGeneration: 4, acceptedAt,
    })
    expect(clearPendingResetAfterLifecycle('user-1', { ...accepted, cleanup_pending: false, operation_id: 'old-op' })).toBe(false)
    expect(clearPendingResetAfterLifecycle('user-1', { ...accepted, generation: 5, cleanup_pending: false })).toBe(false)
    expect(clearPendingResetAfterLifecycle('user-1', { ...accepted, status: 'queued', cleanup_pending: false })).toBe(false)
    expect(clearPendingResetAfterLifecycle('user-1', { ...accepted, status: 'booting', cleanup_pending: true })).toBe(false)
    expect(loadPendingReset('user-1', 'session-1', 3)).not.toBeNull()

    expect(clearPendingResetAfterLifecycle('user-1', { ...accepted, status: 'ready', cleanup_pending: false })).toBe(true)
    expect(loadPendingReset('user-1', 'session-1', 3)).toBeNull()
  })
})
