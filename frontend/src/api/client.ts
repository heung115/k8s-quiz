import { useAuthStore } from '../stores/auth'

const BASE_URL = import.meta.env.VITE_API_URL || ''

// httpOnly cookies (access_token, refresh_token) authenticate every same-origin
// request, so the client sends no Authorization header and holds no tokens in
// memory. On a 401 we refresh exactly once and retry the original request once;
// concurrent 401s share a single in-flight refresh so the rotating refresh
// token is never reused (reuse would revoke the whole token family).
let refreshPromise: Promise<boolean> | null = null

async function doRefresh(): Promise<boolean> {
  try {
    const res = await fetch(`${BASE_URL}/api/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
    })
    return res.ok
  } catch {
    return false
  }
}

function refreshOnce(): Promise<boolean> {
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
    const refreshed = await refreshOnce()
    if (refreshed) {
      res = await fetch(`${BASE_URL}${path}`, { ...options, headers, credentials: 'same-origin' })
    } else {
      // Refresh rejected (missing/invalid/revoked): drop auth so the router
      // guard sends the user to /login.
      useAuthStore.getState().clearAuth()
    }
  }

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'request failed' }))
    throw new Error(err.error || `HTTP ${res.status}`)
  }

  return res.json()
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
}

// Single-flight cookie refresh for non-REST callers (the terminal WS recovers
// an expired access cookie on an idle page through this, sharing the same
// in-flight promise as REST 401s so the rotating refresh token is never reused).
export function refreshAuth(): Promise<boolean> {
  return refreshOnce()
}
