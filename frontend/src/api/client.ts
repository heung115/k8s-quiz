import { useAuthStore } from '../stores/auth'

const BASE_URL = import.meta.env.VITE_API_URL || ''

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const { accessToken, refreshToken, setAuth, logout } = useAuthStore.getState()

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string>),
  }

  if (accessToken) {
    headers['Authorization'] = `Bearer ${accessToken}`
  }

  let res = await fetch(`${BASE_URL}${path}`, { ...options, headers })

  if (res.status === 401 && refreshToken) {
    const refreshRes = await fetch(`${BASE_URL}/api/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: refreshToken }),
    })

    if (refreshRes.ok) {
      const data = await refreshRes.json()
      const user = useAuthStore.getState().user!
      setAuth(user, data.access_token, data.refresh_token)
      headers['Authorization'] = `Bearer ${data.access_token}`
      res = await fetch(`${BASE_URL}${path}`, { ...options, headers })
    } else {
      logout()
      window.location.href = '/login'
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
