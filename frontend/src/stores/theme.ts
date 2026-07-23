import { create } from 'zustand'

export type Theme = 'light' | 'dark'

const STORAGE_KEY = 'k8q-theme'

function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' &&
    window.matchMedia?.('(prefers-color-scheme: dark)').matches
}

// URL override (#theme=light|dark or ?theme=...) lets a shared link preview a
// theme and gives us a hook for verification. Login's token hash only reads
// access_token, so a co-existing theme key is harmless.
function urlTheme(): Theme | null {
  if (typeof window === 'undefined') return null
  const fromHash = new URLSearchParams(window.location.hash.replace(/^#/, '')).get('theme')
  const fromSearch = new URLSearchParams(window.location.search).get('theme')
  const v = fromHash || fromSearch
  return v === 'light' || v === 'dark' ? v : null
}

function storedTheme(): Theme | null {
  if (typeof window === 'undefined') return null
  const v = window.localStorage.getItem(STORAGE_KEY)
  return v === 'light' || v === 'dark' ? v : null
}

function applyTheme(t: Theme) {
  const root = document.documentElement
  root.classList.toggle('theme-light', t === 'light')
  root.classList.toggle('theme-dark', t === 'dark')
  root.style.colorScheme = t
}

const initial: Theme = urlTheme() ?? storedTheme() ?? (systemPrefersDark() ? 'dark' : 'light')
applyTheme(initial)

interface ThemeState {
  theme: Theme
  toggle: () => void
  setTheme: (t: Theme) => void
}

export const useThemeStore = create<ThemeState>((set) => ({
  theme: initial,
  setTheme: (t) => {
    applyTheme(t)
    window.localStorage.setItem(STORAGE_KEY, t)
    set({ theme: t })
  },
  toggle: () => {
    const next = useThemeStore.getState().theme === 'dark' ? 'light' : 'dark'
    applyTheme(next)
    window.localStorage.setItem(STORAGE_KEY, next)
    set({ theme: next })
  },
}))

// follow OS changes only when the user has not made an explicit choice
if (typeof window !== 'undefined') {
  window.matchMedia?.('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
    if (!storedTheme() && !urlTheme()) {
      const t = e.matches ? 'dark' : 'light'
      applyTheme(t)
      useThemeStore.setState({ theme: t })
    }
  })
}
