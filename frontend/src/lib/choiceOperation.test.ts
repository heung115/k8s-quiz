import { beforeEach, describe, expect, it, vi } from 'vitest'
import { APIResponseError } from '../api/client'
import {
  canApplyChoiceResult,
  ChoiceOperationController,
  ChoiceOperationScope,
  clearAllPendingChoices,
  getOrCreatePendingChoice,
  loadPendingChoice,
} from './choiceOperation'

const scope: ChoiceOperationScope = {
  userId: 'user-1',
  problemId: 'problem-1',
  sessionId: 'session-1',
  generation: 3,
  choiceId: 'b',
}

function response(requestID: string, target = scope) {
  return {
    request_id: requestID,
    problem_id: target.problemId,
    session_id: target.sessionId,
    generation: target.generation,
    success: false,
  }
}

describe('choice operation idempotency', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('reuses the exact key after transport failure and component recreation', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const keys: string[] = []
    await expect(new ChoiceOperationController().execute(scope, async (key) => {
      keys.push(key)
      throw new TypeError('connection reset')
    })).rejects.toThrow('connection reset')
    const pending = loadPendingChoice(scope)
    await new ChoiceOperationController().execute(scope, async (key) => {
      keys.push(key)
      return response(key)
    })
    expect(keys).toEqual([pending?.key, pending?.key])
    expect(loadPendingChoice(scope)).toBeNull()
  })

  it('uses a new key after a concrete HTTP response', async () => {
    const randomUUID = vi.fn()
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
    vi.stubGlobal('crypto', { randomUUID })
    const controller = new ChoiceOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      throw new APIResponseError(409, 'stale')
    })).rejects.toMatchObject({ status: 409 })
    await controller.execute(scope, async (key) => {
      keys.push(key)
      return response(key)
    })
    expect(keys[0]).not.toBe(keys[1])
  })

  it('uses distinct durable keys when answer or generation changes', async () => {
    const randomUUID = vi.fn()
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
      .mockReturnValueOnce('33333333-3333-4333-8333-333333333333')
    vi.stubGlobal('crypto', { randomUUID })
    const controller = new ChoiceOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      throw new TypeError('ambiguous')
    })).rejects.toThrow('ambiguous')
    const changedAnswer = { ...scope, choiceId: 'a' }
    await controller.execute(changedAnswer, async (key) => {
      keys.push(key)
      return response(key, changedAnswer)
    })
    const changedGeneration = { ...scope, generation: 4 }
    await controller.execute(changedGeneration, async (key) => {
      keys.push(key)
      return response(key, changedGeneration)
    })
    expect(new Set(keys).size).toBe(3)
  })

  it('shares one request for concurrent clicks in the same exact scope', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const controller = new ChoiceOperationController()
    let resolveRequest!: (value: unknown) => void
    const request = vi.fn(() => new Promise<unknown>((resolve) => { resolveRequest = resolve }))
    const first = controller.execute(scope, request)
    const second = controller.execute(scope, request)
    expect(request).toHaveBeenCalledTimes(1)
    resolveRequest(response('choice:11111111-1111-4111-8111-111111111111'))
    await expect(first).resolves.toMatchObject({ success: false })
    await expect(second).resolves.toMatchObject({ success: false })
  })

  it('retains the key when a 2xx response identity is malformed', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const controller = new ChoiceOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      return { ...response(key), generation: 99 }
    })).rejects.toThrow('올바른 답안 제출 응답')
    await controller.execute(scope, async (key) => {
      keys.push(key)
      return response(key)
    })
    expect(keys[0]).toBe(keys[1])
  })

  it('reset preserves ambiguous storage and clear-all removes every scope', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn()
        .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
        .mockReturnValueOnce('22222222-2222-4222-8222-222222222222'),
    })
    const controller = new ChoiceOperationController()
    await expect(controller.execute(scope, async () => { throw new TypeError('ambiguous') }))
      .rejects.toThrow('ambiguous')
    const pending = loadPendingChoice(scope)
    controller.reset()
    expect(loadPendingChoice(scope)?.key).toBe(pending?.key)
    getOrCreatePendingChoice({ ...scope, choiceId: 'a' })
    clearAllPendingChoices()
    expect(loadPendingChoice(scope)).toBeNull()
    expect(loadPendingChoice({ ...scope, choiceId: 'a' })).toBeNull()
  })

  it('expires a ledger entry after 24 hours', () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const pending = getOrCreatePendingChoice(scope)
    expect(loadPendingChoice(scope, pending.createdAt + 24 * 60 * 60 * 1000)).not.toBeNull()
    expect(loadPendingChoice(scope, pending.createdAt + 24 * 60 * 60 * 1000 + 1)).toBeNull()
  })

  it('rejects a late result after authoritative null or session replacement', () => {
    const terminal = { ...response('choice:terminal'), success: true }
    expect(canApplyChoiceResult(null, scope, terminal)).toBe(false)
    expect(canApplyChoiceResult({
      operation_id: 'operation-1',
      session_id: scope.sessionId,
      problem_id: scope.problemId,
      generation: scope.generation,
      status: 'ready',
      timeout_at: null,
      cleanup_pending: false,
      terminal_reason: null,
      event_sequence: 1,
    }, scope, terminal)).toBe(true)
    expect(canApplyChoiceResult({
      operation_id: 'operation-2',
      session_id: 'new-session',
      problem_id: scope.problemId,
      generation: 1,
      status: 'ready',
      timeout_at: null,
      cleanup_pending: false,
      terminal_reason: null,
      event_sequence: 1,
    }, scope, terminal)).toBe(false)
  })
})
