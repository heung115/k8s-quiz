import { Link, Outlet, useNavigate } from 'react-router-dom'
import { useAuthStore } from '../stores/auth'
import { Container, BookOpen, Shield, LogOut, Trophy } from 'lucide-react'

export function Layout() {
  const { user, logout } = useAuthStore()
  const navigate = useNavigate()

  const handleLogout = () => {
    logout()
    navigate('/login')
  }

  return (
    <div className="min-h-screen flex flex-col">
      <header className="border-b border-edge bg-surface/80 backdrop-blur-sm sticky top-0 z-50">
        <div className="max-w-7xl mx-auto px-4 h-14 flex items-center justify-between">
          <div className="flex items-center gap-6">
            <Link to="/" className="flex items-center gap-2 text-lg font-bold text-accent-hover">
              <Container className="w-5 h-5" aria-hidden="true" />
              K8s Quiz
            </Link>
            <nav className="flex items-center gap-4 text-sm">
              <Link to="/" className="text-ink hover:text-accent-hover transition-colors">Problems</Link>
              <Link to="/leaderboard" className="flex items-center gap-1 text-ink hover:text-accent-hover transition-colors">
                <Trophy className="w-3.5 h-3.5" aria-hidden="true" />
                Leaderboard
              </Link>
              <Link to="/docs" className="flex items-center gap-1 text-ink hover:text-accent-hover transition-colors">
                <BookOpen className="w-3.5 h-3.5" aria-hidden="true" />
                Docs
              </Link>
              {user?.role === 'admin' && (
                <Link to="/admin" className="flex items-center gap-1 text-ink hover:text-accent-hover transition-colors">
                  <Shield className="w-3.5 h-3.5" aria-hidden="true" />
                  Admin
                </Link>
              )}
            </nav>
          </div>
          <div className="flex items-center gap-3">
            <Link to="/profile" className="flex items-center gap-2 text-sm text-ink hover:text-accent-hover">
              {user?.avatar_url && (
                <img src={user.avatar_url} alt="" className="w-6 h-6 rounded-full" />
              )}
              {user?.username}
            </Link>
            <button
              onClick={handleLogout}
              className="flex items-center gap-1 text-xs text-ink-faint hover:text-danger transition-colors"
              aria-label="Logout"
            >
              <LogOut className="w-3.5 h-3.5" aria-hidden="true" />
              Logout
            </button>
          </div>
        </div>
      </header>
      <main className="flex-1">
        <Outlet />
      </main>
    </div>
  )
}
