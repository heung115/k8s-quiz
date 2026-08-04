import { beforeEach, describe, expect, it, vi } from 'vitest'
import { APIResponseError } from '../api/client'
import {
  canApplyEndResult,
  EndOperationController,
  EndOperationScope,
  getOrCreatePendingEnd,
  isDefinitiveEndRejection,
  loadPendingEnd,
  loadPendingEndForProblem,
  parseEndResponse,
} from './endOperation'
import type { Session } from '../types'

const scope: EndOperationScope = {
  userId: 'user-1', problemId: 'p1', sessionId: 'session-1', generation: 2,
}

beforeEach(() => {
  sessionStorage.clear()
  vi.restoreAllMocks()
})

describe('durable end operation', () => {
  it('validates completed and exact cleanup-pending responses', () => {
    expect(parseEndResponse(undefined, scope, 'end:key-0001')).toEqual({ state: 'completed' })
    expect(parseEndResponse({
      request_id: 'end:key-0001', session_id: 'session-1', generation: 2, status: 'cleanup_pending',
    }, scope, 'end:key-0001')).toMatchObject({ state: 'pending' })
    expect(parseEndResponse({
      request_id: 'end:key-0001', session_id: 'session-1', generation: 3, status: 'cleanup_pending',
    }, scope, 'end:key-0001')).toBeNull()
  })

  it('keeps one persisted key across 202 and transport retries until 204', async () => {
    const controller = new EndOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      return { request_id: key, session_id: scope.sessionId, generation: scope.generation, status: 'cleanup_pending' }
    })).resolves.toMatchObject({ state: 'pending' })
    expect(loadPendingEnd(scope)?.key).toBe(keys[0])

    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      throw new TypeError('network lost')
    })).rejects.toThrow('network lost')
    expect(keys[1]).toBe(keys[0])
    expect(loadPendingEnd(scope)?.key).toBe(keys[0])

    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      return undefined
    })).resolves.toEqual({ state: 'completed' })
    expect(keys[2]).toBe(keys[0])
    expect(loadPendingEnd(scope)).toBeNull()
  })

  it('single-flights one exact scope', async () => {
    const controller = new EndOperationController()
    let release!: (value: unknown) => void
    const request = vi.fn(() => new Promise((resolve) => { release = resolve }))
    const first = controller.execute(scope, request)
    const duplicate = controller.execute(scope, request)
    expect(request).toHaveBeenCalledTimes(1)
    release(undefined)
    await expect(Promise.all([first, duplicate])).resolves.toEqual([
      { state: 'completed' }, { state: 'completed' },
    ])
  })

  it('finds the newest persisted operation for one user and problem', () => {
    const first = getOrCreatePendingEnd(scope)
    const newerScope = { ...scope, sessionId: 'session-2', generation: 1 }
    vi.spyOn(Date, 'now').mockReturnValue(first.createdAt + 1)
    const newer = getOrCreatePendingEnd(newerScope)
    getOrCreatePendingEnd({ ...scope, userId: 'user-2', sessionId: 'other-user' })

    expect(loadPendingEndForProblem(scope.userId, scope.problemId)).toEqual(newer)
    expect(loadPendingEndForProblem('missing-user', scope.problemId)).toBeNull()
  })

  it('applies pending only to the exact allocation and completed only without a replacement', () => {
    const exact: Session = {
      operation_id: 'operation-1',
      session_id: scope.sessionId,
      problem_id: scope.problemId,
      generation: scope.generation,
      status: 'ready',
      timeout_at: null,
      cleanup_pending: false,
      terminal_reason: null,
      event_sequence: 1,
    }
    const pending = {
      state: 'pending' as const,
      response: {
        request_id: 'end:key-0001', session_id: scope.sessionId,
        generation: scope.generation, status: 'cleanup_pending' as const,
      },
    }
    expect(canApplyEndResult(exact, scope, pending)).toBe(true)
    expect(canApplyEndResult({ ...exact, generation: scope.generation + 1 }, scope, pending)).toBe(false)
    expect(canApplyEndResult({ ...exact, session_id: 'replacement' }, scope, { state: 'completed' })).toBe(false)
    expect(canApplyEndResult(null, scope, { state: 'completed' })).toBe(false)
    expect(canApplyEndResult(null, scope, { state: 'completed' }, true)).toBe(true)
  })

  it('discards a rejected 409 key but retains an ambiguous 500 key', async () => {
    const controller = new EndOperationController()
    const first = getOrCreatePendingEnd(scope).key
    await expect(controller.execute(scope, async () => {
      throw new APIResponseError(409, 'stale')
    })).rejects.toThrow('stale')
    expect(loadPendingEnd(scope)).toBeNull()

    const second = getOrCreatePendingEnd(scope).key
    expect(second).not.toBe(first)
    await expect(controller.execute(scope, async () => {
      throw new APIResponseError(500, 'unknown')
    })).rejects.toThrow('unknown')
    expect(loadPendingEnd(scope)?.key).toBe(second)
  })

  it('retains ambiguous 408 and clears only known definitive rejections', async () => {
    expect(isDefinitiveEndRejection(new APIResponseError(408, 'timeout'))).toBe(false)
    expect(isDefinitiveEndRejection(new APIResponseError(429, 'busy'))).toBe(false)
    expect(isDefinitiveEndRejection(new APIResponseError(500, 'unknown'))).toBe(false)
    for (const status of [400, 401, 403, 404, 409]) {
      expect(isDefinitiveEndRejection(new APIResponseError(status, 'rejected'))).toBe(true)
    }

    const controller = new EndOperationController()
    const key = getOrCreatePendingEnd(scope).key
    await expect(controller.execute(scope, async () => {
      throw new APIResponseError(408, 'gateway timeout')
    })).rejects.toMatchObject({ status: 408 })
    expect(loadPendingEnd(scope)?.key).toBe(key)
  })
})
