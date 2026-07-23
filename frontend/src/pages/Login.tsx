import { useEffect } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuthStore } from '../stores/auth'
import { SectionLabel } from '../components/ui'
import { ChevronLeft } from 'lucide-react'

function GithubMark({ className = 'w-5 h-5' }: { className?: string }) {
  return (
    <svg className={className} fill="currentColor" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.122 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>
    </svg>
  )
}

const ERROR_TEXT: Record<string, string> = {
  invalid_state: '세션이 만료되었습니다. 다시 시도해주세요.',
  missing_code: '인증 코드가 없습니다. 다시 시도해주세요.',
  auth_failed: 'GitHub 인증에 실패했습니다.',
}

export function Login() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const { setAuth, user } = useAuthStore()

  useEffect(() => {
    const accessToken = params.get('access_token')
    const refreshToken = params.get('refresh_token')
    const userStr = params.get('user')
    if (accessToken && userStr) {
      try {
        const userData = JSON.parse(decodeURIComponent(userStr))
        setAuth(userData, accessToken, refreshToken || '')
        navigate('/')
      } catch { /* fall through */ }
    }
  }, [params])

  useEffect(() => {
    if (user) navigate('/')
  }, [user])

  const error = params.get('error')

  return (
    <div className="min-h-screen bg-canvas bg-grid flex items-center justify-center px-5">
      <div className="w-full max-w-md">
        <Link to="/" className="micro text-ink-faint hover:text-ink transition-colors inline-flex items-center gap-1 mb-8">
          <ChevronLeft className="w-3.5 h-3.5" aria-hidden="true" /> 랜딩으로
        </Link>

        <div className="border border-edge bg-surface shadow-[0_24px_80px_-24px_rgba(50,108,229,0.25)]">
          <div className="px-8 pt-8 pb-6 border-b border-edge-soft">
            <div className="flex items-center gap-3">
              <span className="w-11 h-11 bg-accent text-white flex items-center justify-center text-2xl shadow-[0_0_24px_rgba(50,108,229,0.5)]" aria-hidden="true">⎈</span>
              <div>
                <p className="font-display font-bold text-2xl leading-tight">K8S<span className="text-accent-hover">QUIZ</span></p>
                <p className="micro text-ink-faint">OPERATOR SIGN-IN</p>
              </div>
            </div>
          </div>

          <div className="px-8 py-7">
            <SectionLabel className="mb-5">AUTH REQUIRED</SectionLabel>

            {error && (
              <div className="mb-5 border border-danger/40 bg-danger-soft px-4 py-3 text-sm text-danger">
                {ERROR_TEXT[error] || '로그인 중 오류가 발생했습니다.'}
              </div>
            )}

            <button
              onClick={() => { window.location.href = '/api/auth/github' }}
              className="btn-primary w-full py-3"
            >
              <GithubMark className="w-5 h-5" /> GitHub으로 로그인
            </button>

            <div className="mt-7 font-mono text-[11px] text-ink-faint leading-relaxed border-t border-edge-soft pt-5">
              <p><span className="text-accent">$</span> 로그인하면 GitHub 프로필(이름·아바타)만 가져옵니다.</p>
              <p className="mt-1"><span className="text-accent">$</span> 세션은 JWT로 유지되며 언제든 로그아웃할 수 있습니다.</p>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
