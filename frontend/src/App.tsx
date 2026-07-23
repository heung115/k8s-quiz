import { useEffect, lazy, Suspense } from 'react'
import { Routes, Route, Navigate } from 'react-router-dom'
import { useAuthStore } from './stores/auth'
import { Layout } from './components/Layout'
import { ErrorBoundary } from './components/ErrorBoundary'

const Landing     = lazy(() => import('./pages/Landing').then((m) => ({ default: m.Landing })))
const Dashboard   = lazy(() => import('./pages/Dashboard').then((m) => ({ default: m.Dashboard })))
const Login       = lazy(() => import('./pages/Login').then((m) => ({ default: m.Login })))
const Problems    = lazy(() => import('./pages/Problems').then((m) => ({ default: m.Problems })))
const ProblemPage = lazy(() => import('./pages/ProblemPage').then((m) => ({ default: m.ProblemPage })))
const Profile     = lazy(() => import('./pages/Profile').then((m) => ({ default: m.Profile })))
const Admin       = lazy(() => import('./pages/Admin').then((m) => ({ default: m.Admin })))
const Docs        = lazy(() => import('./pages/Docs').then((m) => ({ default: m.Docs })))
const Leaderboard = lazy(() => import('./pages/Leaderboard').then((m) => ({ default: m.Leaderboard })))
const NotFound    = lazy(() => import('./pages/NotFound').then((m) => ({ default: m.NotFound })))

function SuspenseWrap({ children }: { children: React.ReactNode }) {
  return (
    <Suspense fallback={<div className="min-h-[60vh] flex items-center justify-center font-mono text-[10px] text-ink-faint">loading…</div>}>
      {children}
    </Suspense>
  )
}

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { user } = useAuthStore()
  if (!user) return <Navigate to="/login" replace />
  return <>{children}</>
}

function Root() {
  const { user } = useAuthStore()
  return user ? <Layout><Dashboard /></Layout> : <Landing />
}

export default function App() {
  const { loadFromStorage } = useAuthStore()
  useEffect(() => { loadFromStorage() }, [])

  return (
    <ErrorBoundary>
      <SuspenseWrap>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/" element={<Root />} />
          <Route element={<ProtectedRoute><Layout /></ProtectedRoute>}>
            <Route path="/problems" element={<Problems />} />
            <Route path="/problems/:id" element={<ProblemPage />} />
            <Route path="/profile" element={<Profile />} />
            <Route path="/admin" element={<Admin />} />
            <Route path="/leaderboard" element={<Leaderboard />} />
          </Route>
          <Route path="*" element={<NotFound />} />
        </Routes>
      </SuspenseWrap>
    </ErrorBoundary>
  )
}
