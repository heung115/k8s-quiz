import { useThemeStore } from '../stores/theme'
import { Sun, Moon } from 'lucide-react'

export function ThemeToggle({ className = '' }: { className?: string }) {
  const { theme, toggle } = useThemeStore()
  const isDark = theme === 'dark'
  return (
    <button
      onClick={toggle}
      aria-label={isDark ? '라이트 테마로 전환' : '다크 테마로 전환'}
      title={isDark ? '라이트 테마' : '다크 테마'}
      className={`relative inline-flex items-center justify-center w-8 h-8 border border-edge text-ink-muted hover:text-ink hover:border-accent/60 transition-colors ${className}`}
    >
      {isDark
        ? <Sun className="w-4 h-4" aria-hidden="true" />
        : <Moon className="w-4 h-4" aria-hidden="true" />}
    </button>
  )
}
