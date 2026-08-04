import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { api, ensureWebSocketAuth } from './client'
import { useAuthStore } from '../stores/auth'
import type { User } from '../types'
import type { Session } from '../types'
import { getOrCreatePendingEnd, loadPendingEnd } from '../lib/endOperation'
import { useSessionStore } from '../stores/session'

// Minimal Response-like object: the client only reads `.status`/`.ok` and calls
// `.json()`, so a plain object avoids undici Response internals in tests.
function res(status: number, body: unknown = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  }
}

const REFRESH = '/api/auth/refresh'
const DATA = '/api/things'
const AUTH_ME = '/api/auth/me'
const activeSession: Session = {
  operation_id: 'operation-1',
  session_id: 'session-1',
  problem_id: 'p1',
  generation: 1,
  status: 'ready',
  timeout_at: null,
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 1,
}

describe('api client — 401 → refresh → retry contract', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    // Isolate the auth singleton so clearAuth assertions are meaningful.
    useAuthStore.setState({ user: null, bootstrapped: true })
    useSessionStore.getState().clear()
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  // Counted after the fact (mock.calls is reliable once promises settle).
  const refreshCalls = () =>
    fetchMock.mock.calls.filter((c) => String(c[0]).endsWith(REFRESH)).length
  const dataCalls = () =>
    fetchMock.mock.calls.filter((c) => String(c[0]).endsWith(DATA)).length

  it('(a) passes a 200 straight through without refreshing', async () => {
    fetchMock.mockImplementation(async () => res(200, { hello: 'world' }))
    await expect(api.get(DATA)).resolves.toEqual({ hello: 'world' })
    expect(refreshCalls()).toBe(0)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('(b) on 401 refreshes once, retries the original once, returns the retried body', async () => {
    let dataHits = 0
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(200, { user: {} })
      dataHits += 1
      return dataHits === 1 ? res(401, { error: 'expired' }) : res(200, { fresh: true })
    })
    await expect(api.get(DATA)).resolves.toEqual({ fresh: true })
    expect(refreshCalls()).toBe(1)
    expect(dataCalls()).toBe(2) // original + one retry
  })

  it('(c) concurrent 401s share ONE in-flight refresh (AUTH-5 single-flight)', async () => {
    let refreshed = false
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) {
        // Stay in-flight briefly so every concurrent 401 coalesces onto this
        // single refresh rather than each starting its own (which would reuse
        // the rotating refresh token and revoke the whole family).
        await new Promise((r) => setTimeout(r, 5))
        refreshed = true
        return res(200, { user: {} })
      }
      return refreshed ? res(200, { ok: 1 }) : res(401, { error: 'expired' })
    })
    const results = await Promise.all([
      api.get(DATA),
      api.get(DATA),
      api.get(DATA),
      api.get(DATA),
    ])
    expect(results).toEqual([{ ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }])
    expect(refreshCalls()).toBe(1) // exactly one refresh across all four
  })

  it('(d) when the refresh itself 401s, clears the auth store and does not retry', async () => {
    useAuthStore.setState({ user: { id: 'u1' } as unknown as User })
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(401, { error: 'revoked' })
      return res(401, { error: 'expired' })
    })
    await expect(api.get(DATA)).rejects.toThrow('expired')
    expect(refreshCalls()).toBe(1)
    expect(dataCalls()).toBe(1) // no retry after a failed refresh
    expect(useAuthStore.getState().user).toBeNull()
  })

  it('(e) non-401 errors (429) do not trigger a refresh', async () => {
    fetchMock.mockImplementation(async () => res(429, { error: 'too many requests' }))
    await expect(api.get(DATA)).rejects.toThrow('too many requests')
    expect(refreshCalls()).toBe(0)
    expect(dataCalls()).toBe(1)
  })

  it('(f) preserves Idempotency-Key across a 401 refresh retry', async () => {
    let dataHits = 0
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(200, { user: {} })
      dataHits += 1
      return dataHits === 1 ? res(401, { error: 'expired' }) : res(200, { ok: true })
    })
    await api.post(DATA, undefined, { 'Idempotency-Key': 'verify:s1:1:stable-key' })
    const attempts = fetchMock.mock.calls.filter((c) => String(c[0]).endsWith(DATA))
    expect(attempts).toHaveLength(2)
    for (const [, init] of attempts) {
      expect((init as RequestInit).headers).toMatchObject({
        'Idempotency-Key': 'verify:s1:1:stable-key',
      })
    }
  })

  it('(g) accepts an empty 204 response without attempting JSON decoding', async () => {
    const json = vi.fn(async () => { throw new Error('empty body') })
    fetchMock.mockImplementation(async () => ({ ok: true, status: 204, json }))
    await expect(api.delete('/api/sessions/current')).resolves.toBeUndefined()
    expect(json).not.toHaveBeenCalled()
  })

  it('(h) decodes a 202 cleanup-pending acknowledgement', async () => {
    fetchMock.mockImplementation(async () => res(202, { status: 'cleanup_pending' }))
    await expect(api.delete('/api/sessions/current')).resolves.toEqual({ status: 'cleanup_pending' })
  })

  it('(i) probes WebSocket auth without refreshing a valid cookie', async () => {
    fetchMock.mockImplementation(async () => res(200, { id: 'u1' }))
    await expect(ensureWebSocketAuth()).resolves.toBe(true)
    expect(refreshCalls()).toBe(0)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('(j) refreshes an expired WebSocket cookie and retries the probe', async () => {
    let probes = 0
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(200)
      if (String(url).endsWith(AUTH_ME)) {
        probes += 1
        return probes === 1 ? res(401, { error: 'expired' }) : res(200, { id: 'u1' })
      }
      return res(500)
    })
    await expect(ensureWebSocketAuth()).resolves.toBe(true)
    expect(refreshCalls()).toBe(1)
    expect(probes).toBe(2)
  })

  it('(k) shares one refresh across concurrent REST and WebSocket probes', async () => {
    let refreshed = false
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) {
        await new Promise((resolve) => setTimeout(resolve, 5))
        refreshed = true
        return res(200)
      }
      return refreshed ? res(200, { ok: true }) : res(401, { error: 'expired' })
    })
    const [rest, terminal, lifecycle] = await Promise.all([
      api.get(DATA), ensureWebSocketAuth(), ensureWebSocketAuth(),
    ])
    expect(rest).toEqual({ ok: true })
    expect(terminal).toBe(true)
    expect(lifecycle).toBe(true)
    expect(refreshCalls()).toBe(1)
  })

  it('(l) clears auth after final 401 but preserves it on transient probe failure', async () => {
    useAuthStore.setState({ user: { id: 'u1' } as unknown as User })
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(401, { error: 'revoked' })
      return res(401, { error: 'expired' })
    })
    await expect(ensureWebSocketAuth()).resolves.toBe(false)
    expect(useAuthStore.getState().user).toBeNull()

    useAuthStore.setState({ user: { id: 'u1' } as unknown as User })
    fetchMock.mockImplementation(async () => res(503, { error: 'temporary' }))
    await expect(ensureWebSocketAuth()).resolves.toBe(false)
    expect(useAuthStore.getState().user?.id).toBe('u1')
  })

  it.each([
    ['refresh 503', async () => res(503, { error: 'refresh temporarily unavailable' })],
    ['refresh network failure', async () => { throw new TypeError('refresh network failed') }],
  ])('(m) preserves auth, session, and durable ledgers on %s', async (_name, refreshResponse) => {
    const user = { id: 'u1' } as User
    useAuthStore.setState({ user })
    useSessionStore.setState({ session: { ...activeSession }, stage: 'ready' })
    const scope = { userId: 'u1', problemId: 'p1', sessionId: 'session-1', generation: 1 }
    const pending = getOrCreatePendingEnd(scope)
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return refreshResponse()
      return res(401, { error: 'expired' })
    })

    await expect(api.get(DATA)).rejects.toThrow()

    expect(useAuthStore.getState().user?.id).toBe('u1')
    expect(useSessionStore.getState().session).toMatchObject({ session_id: 'session-1', generation: 1 })
    expect(loadPendingEnd(scope)?.key).toBe(pending.key)
    expect(dataCalls()).toBe(1)
    expect(refreshCalls()).toBe(1)
  })

  it.each([400, 401, 403])('(n) clears auth after definitive refresh %s', async (status) => {
    useAuthStore.setState({ user: { id: 'u1' } as User })
    fetchMock.mockImplementation(async (url: string) => {
      if (String(url).endsWith(REFRESH)) return res(status, { error: 'refresh rejected' })
      return res(401, { error: 'expired' })
    })

    await expect(api.get(DATA)).rejects.toThrow('expired')
    expect(useAuthStore.getState().user).toBeNull()
  })
})
