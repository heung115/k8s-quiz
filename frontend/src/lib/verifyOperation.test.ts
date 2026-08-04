import { beforeEach, describe, expect, it, vi } from 'vitest'
import { APIResponseError } from '../api/client'
import {
  clearAllPendingVerifies,
  getOrCreatePendingVerify,
  loadPendingVerify,
  VerifyOperationController,
  VerifyOperationScope,
} from './verifyOperation'

const scope: VerifyOperationScope = {
  userId: 'user-1',
  problemId: 'problem-1',
  sessionId: 'session-1',
  generation: 3,
}

describe('verify operation idempotency', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('uses a new key after a concrete HTTP response', async () => {
    const randomUUID = vi.fn()
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
    vi.stubGlobal('crypto', { randomUUID })
    const controller = new VerifyOperationController()
    const keys: string[] = []

    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      throw new APIResponseError(503, 'verification unavailable')
    })).rejects.toMatchObject({ status: 503 })
    await controller.execute(scope, async (key) => {
      keys.push(key)
      return { success: false, log: 'still broken' }
    })

    expect(keys[0]).not.toBe(keys[1])
    expect(loadPendingVerify(scope)).toBeNull()
  })

  it('reuses the exact key after transport failure and component recreation', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const keys: string[] = []
    await expect(new VerifyOperationController().execute(scope, async (key) => {
      keys.push(key)
      throw new TypeError('connection reset')
    })).rejects.toThrow('connection reset')

    const persisted = loadPendingVerify(scope)
    await new VerifyOperationController().execute(scope, async (key) => {
      keys.push(key)
      return { success: true, log: 'passed' }
    })

    expect(keys).toEqual([persisted?.key, persisted?.key])
    expect(loadPendingVerify(scope)).toBeNull()
  })

  it('reset drops only in-memory sharing and preserves the durable key', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const controller = new VerifyOperationController()
    await expect(controller.execute(scope, async () => {
      throw new TypeError('ambiguous')
    })).rejects.toThrow('ambiguous')
    const pending = loadPendingVerify(scope)

    controller.reset()
    await controller.execute(scope, async (key) => {
      expect(key).toBe(pending?.key)
      return { success: true, log: '' }
    })
  })

  it('retains the key for a malformed successful response', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const controller = new VerifyOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      return { success: true }
    })).rejects.toThrow('올바른 검증 응답')
    await controller.execute(scope, async (key) => {
      keys.push(key)
      return { success: true, log: '' }
    })
    expect(keys[0]).toBe(keys[1])
  })

  it('does not carry an ambiguous key into another exact scope', async () => {
    const randomUUID = vi.fn()
      .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
    vi.stubGlobal('crypto', { randomUUID })
    const controller = new VerifyOperationController()
    const keys: string[] = []
    await expect(controller.execute(scope, async (key) => {
      keys.push(key)
      throw new TypeError('ambiguous')
    })).rejects.toThrow('ambiguous')
    await controller.execute({ ...scope, generation: 4 }, async (key) => {
      keys.push(key)
      return { success: false, log: '' }
    })
    expect(new Set(keys).size).toBe(2)
  })

  it('shares one in-flight request for concurrent clicks', async () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const controller = new VerifyOperationController()
    let resolveRequest!: (value: unknown) => void
    const request = vi.fn(() => new Promise<unknown>((resolve) => { resolveRequest = resolve }))
    const first = controller.execute(scope, request)
    const second = controller.execute(scope, request)
    expect(request).toHaveBeenCalledTimes(1)
    resolveRequest({ success: true, log: 'passed' })
    await expect(first).resolves.toMatchObject({ success: true })
    await expect(second).resolves.toMatchObject({ success: true })
  })

  it('expires malformed, future, and older-than-24h ledger entries', () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn().mockReturnValue('11111111-1111-4111-8111-111111111111'),
    })
    const pending = getOrCreatePendingVerify(scope)
    expect(loadPendingVerify(scope, pending.createdAt + 24 * 60 * 60 * 1000)).not.toBeNull()
    expect(loadPendingVerify(scope, pending.createdAt + 24 * 60 * 60 * 1000 + 1)).toBeNull()
  })

  it('clears every verify ledger on auth teardown', () => {
    vi.stubGlobal('crypto', {
      randomUUID: vi.fn()
        .mockReturnValueOnce('11111111-1111-4111-8111-111111111111')
        .mockReturnValueOnce('22222222-2222-4222-8222-222222222222'),
    })
    getOrCreatePendingVerify(scope)
    getOrCreatePendingVerify({ ...scope, generation: 4 })
    clearAllPendingVerifies()
    expect(loadPendingVerify(scope)).toBeNull()
    expect(loadPendingVerify({ ...scope, generation: 4 })).toBeNull()
  })
})
