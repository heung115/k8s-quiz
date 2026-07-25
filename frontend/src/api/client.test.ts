import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { api } from './client'
import { useAuthStore } from '../stores/auth'
import type { User } from '../types'

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

describe('api client — 401 → refresh → retry contract', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    // Isolate the auth singleton so clearAuth assertions are meaningful.
    useAuthStore.setState({ user: null, bootstrapped: true })
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
})
