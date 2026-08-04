import { create } from 'zustand'
import { User } from '../types'
import { api } from '../api/client'
import { clearAllPendingStarts } from '../lib/startOperation'
import { clearAllPendingResets } from '../lib/resetOperation'
import { clearAllPendingEnds } from '../lib/endOperation'
import { clearAllPendingVerifies } from '../lib/verifyOperation'
import { clearAllPendingChoices } from '../lib/choiceOperation'
import { clearAllLifecycleCursors } from '../lib/lifecycleCursor'
import { useSessionStore } from './session'

function clearPendingOperations() {
  clearAllPendingStarts()
  clearAllPendingResets()
  clearAllPendingEnds()
  clearAllPendingVerifies()
  clearAllPendingChoices()
  clearAllLifecycleCursors()
}

function clearUserRuntimeState() {
  clearPendingOperations()
  useSessionStore.getState().clear()
}

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
  setUser: (user) => set((state) => {
    if (state.user && state.user.id !== user?.id) clearUserRuntimeState()
    return { user }
  }),
  // Called by the API client when token refresh fails: drop the in-memory user
  // so the router guard redirects to /login. No tokens live in memory.
  clearAuth: () => {
	clearUserRuntimeState()
	set({ user: null })
  },
  bootstrap: async () => {
    try {
      // Same-origin httpOnly cookies ride along; the client transparently
      // refreshes once on 401 before this rejects.
      const user = await api.get<User>('/api/auth/me')
      set({ user, bootstrapped: true })
    } catch {
      // request() owns definitive authentication teardown. A transport/5xx
      // failure is ambiguous, so bootstrap must retain the current identity,
      // session, and durable operation ledgers for a later retry.
      set({ bootstrapped: true })
    }
  },
  logout: () => {
    // Best-effort server-side revocation (clears the httpOnly cookies); the
    // local user is cleared regardless of the outcome.
    fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' }).catch(() => {})
	clearUserRuntimeState()
	set({ user: null })
  },
}))
