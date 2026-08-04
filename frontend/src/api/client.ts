import { useAuthStore } from '../stores/auth'

const BASE_URL = import.meta.env.VITE_API_URL || ''

export class APIResponseError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'APIResponseError'
    this.status = status
  }
}

// httpOnly cookies (access_token, refresh_token) authenticate every same-origin
// request, so the client sends no Authorization header and holds no tokens in
// memory. On a 401 we refresh exactly once and retry the original request once;
// concurrent 401s share a single in-flight refresh so the rotating refresh
// token is never reused (reuse would revoke the whole token family).
export type RefreshResult =
  | { state: 'refreshed' }
  | { state: 'unauthorized' }
  | { state: 'transient'; error: Error }

let refreshPromise: Promise<RefreshResult> | null = null

async function responseError(res: Response): Promise<APIResponseError> {
  const body = await res.json().catch(() => ({ error: 'request failed' })) as { error?: unknown }
  const message = typeof body.error === 'string' && body.error.length > 0
    ? body.error
    : `HTTP ${res.status}`
  return new APIResponseError(res.status, message)
}

async function doRefresh(): Promise<RefreshResult> {
  try {
    const res = await fetch(`${BASE_URL}/api/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
    })
    if (res.ok) return { state: 'refreshed' }
    if ([400, 401, 403].includes(res.status)) return { state: 'unauthorized' }
    return { state: 'transient', error: await responseError(res) }
  } catch (error) {
    return {
      state: 'transient',
      error: error instanceof Error ? error : new Error('authentication refresh failed'),
    }
  }
}

function refreshOnce(): Promise<RefreshResult> {
  if (!refreshPromise) {
    refreshPromise = doRefresh().finally(() => {
      refreshPromise = null
    })
  }
  return refreshPromise
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string>),
  }

  let res = await fetch(`${BASE_URL}${path}`, { ...options, headers, credentials: 'same-origin' })

  if (res.status === 401 && !path.startsWith('/api/auth/refresh')) {
    const refresh = await refreshOnce()
    if (refresh.state === 'refreshed') {
      res = await fetch(`${BASE_URL}${path}`, { ...options, headers, credentials: 'same-origin' })
      if (res.status === 401) useAuthStore.getState().clearAuth()
    } else if (refresh.state === 'unauthorized') {
      // Refresh rejected (missing/invalid/revoked): drop auth so the router
      // guard sends the user to /login.
      useAuthStore.getState().clearAuth()
    } else {
      // A refresh transport/5xx failure does not prove the cookie family is
      // invalid. Preserve the user and every durable retry ledger so the exact
      // operation can be replayed after the outage.
      throw refresh.error
    }
  }

  if (!res.ok) {
    throw await responseError(res)
  }

	if (res.status === 204 || res.status === 205) return undefined as T
  return res.json()
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown, headers?: Record<string, string>) =>
    request<T>(path, { method: 'POST', headers, body: body ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
}

// Single-flight cookie refresh for non-REST callers (the terminal WS recovers
// an expired access cookie on an idle page through this, sharing the same
// in-flight promise as REST 401s so the rotating refresh token is never reused).
export function refreshAuth(): Promise<RefreshResult> {
  return refreshOnce()
}

// WebSocket upgrades cannot expose their HTTP 401 response to browser code.
// Probe the same cookie through the REST client first so an expired access
// cookie joins the shared single-flight refresh used by ordinary requests.
// Transient network/5xx failures leave auth intact and return false; a final
// 401 clears auth inside request().
export async function ensureWebSocketAuth(): Promise<boolean> {
  try {
    await request<unknown>('/api/auth/me')
    return true
  } catch {
    return false
  }
}
