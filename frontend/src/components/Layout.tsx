import { Link, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useAuthStore } from '../stores/auth'
import { BookOpen, Shield, LogOut, Trophy, ListChecks } from 'lucide-react'
import { ThemeToggle } from './ThemeToggle'

export function Layout({ children }: { children?: React.ReactNode }) {
  const { user, logout } = useAuthStore()
  const navigate = useNavigate()
  const location = useLocation()

  const handleLogout = () => {
    logout()
    navigate('/')
  }

  const navItem = (to: string, label: string, icon?: React.ReactNode) => {
    const active = to === '/' ? location.pathname === '/' : location.pathname.startsWith(to)
    return (
      <Link
        to={to}
        aria-current={active ? 'page' : undefined}
        className={`flex items-center gap-1.5 px-3 py-1.5 text-sm transition-colors border-b-2 -mb-px ${
          active
            ? 'text-ink border-accent'
            : 'text-ink-muted border-transparent hover:text-ink hover:border-edge'
        }`}
      >
        {icon}
        {label}
      </Link>
    )
  }

  return (
    <div className="min-h-screen flex flex-col bg-canvas bg-grid">
      <header className="border-b border-edge bg-canvas/90 backdrop-blur sticky top-0 z-50">
        <div className="max-w-7xl mx-auto px-5 h-14 flex items-center justify-between gap-4">
          <div className="flex items-center gap-1 min-w-0">
            <Link to="/" className="flex items-center gap-2 font-display font-bold mr-4 shrink-0">
              <span className="w-7 h-7 bg-accent text-white flex items-center justify-center text-base shadow-[0_0_16px_rgba(50,108,229,0.45)]" aria-hidden="true">⎈</span>
              <span className="hidden sm:inline">K8S<span className="text-accent-hover">QUIZ</span></span>
            </Link>
            <nav className="flex items-center overflow-x-auto" aria-label="Main">
              {navItem('/problems', '문제', <ListChecks className="w-3.5 h-3.5" aria-hidden="true" />)}
              {navItem('/leaderboard', '리더보드', <Trophy className="w-3.5 h-3.5" aria-hidden="true" />)}
              {navItem('/docs', '문서', <BookOpen className="w-3.5 h-3.5" aria-hidden="true" />)}
              {user?.role === 'admin' && navItem('/admin', '관리', <Shield className="w-3.5 h-3.5" aria-hidden="true" />)}
            </nav>
          </div>
          <div className="flex items-center gap-3 shrink-0">
            <Link to="/profile" className="flex items-center gap-2 text-sm text-ink-muted hover:text-ink transition-colors">
              {user?.avatar_url ? (
                <img src={user.avatar_url} alt="" className="w-6 h-6 rounded-full border border-edge" />
              ) : (
                <span className="w-6 h-6 rounded-full border border-edge bg-surface-2 flex items-center justify-center micro text-ink-faint">
                  {user?.username?.[0]?.toUpperCase()}
                </span>
              )}
              <span className="hidden sm:inline font-mono text-xs">{user?.username}</span>
            </Link>
            <ThemeToggle />
            <button
              onClick={handleLogout}
              className="text-ink-faint hover:text-danger transition-colors p-1.5"
              aria-label="로그아웃"
              title="로그아웃"
            >
              <LogOut className="w-4 h-4" aria-hidden="true" />
            </button>
          </div>
        </div>
      </header>
      <main className="flex-1">{children ?? <Outlet />}</main>
    </div>
  )
}
