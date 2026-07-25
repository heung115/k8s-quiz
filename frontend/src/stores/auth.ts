import { create } from 'zustand'
import { User } from '../types'
import { api } from '../api/client'

interface AuthState {
  user: User | null
  // False until the initial GET /api/auth/me resolves, so route guards can show
  // a loading state instead of flashing /login on a hard refresh.
  bootstrapped: boolean
  setUser: (user: User | null) => void
  clearAuth: () => void
  bootstrap: () => Promise<void>
  logout: () => void
}

export const useAuthStore = create<AuthState>((set) => ({
  user: null,
  bootstrapped: false,
  setUser: (user) => set({ user }),
  // Called by the API client when token refresh fails: drop the in-memory user
  // so the router guard redirects to /login. No tokens live in memory.
  clearAuth: () => set({ user: null }),
  bootstrap: async () => {
    try {
      // Same-origin httpOnly cookies ride along; the client transparently
      // refreshes once on 401 before this rejects.
      const user = await api.get<User>('/api/auth/me')
      set({ user, bootstrapped: true })
    } catch {
      set({ user: null, bootstrapped: true })
    }
  },
  logout: () => {
    // Best-effort server-side revocation (clears the httpOnly cookies); the
    // local user is cleared regardless of the outcome.
    fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' }).catch(() => {})
    set({ user: null })
  },
}))
