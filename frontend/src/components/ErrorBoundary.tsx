import { Component, type ReactNode } from 'react'
import { Link } from 'react-router-dom'

interface State { error: Error | null }

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null }
  static getDerivedStateFromError(error: Error) { return { error } }
  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // surface to console for diagnostics; no user data leaked
    console.error('[ErrorBoundary]', error.message, info.componentStack)
  }
  render() {
    if (!this.state.error) return this.props.children
    return (
      <div className="min-h-[60vh] flex items-center justify-center px-5">
        <div className="border border-danger/40 bg-surface max-w-md w-full p-6">
          <p className="font-mono text-[10px] uppercase tracking-wider text-danger">render error</p>
          <p className="mt-2 text-sm text-ink-muted">이 화면을 그리다 오류가 발생했습니다. 다른 페이지는 영향 받지 않습니다.</p>
          <pre className="mt-3 font-mono text-[11px] text-ink-faint whitespace-pre-wrap break-words max-h-32 overflow-auto">{this.state.error.message}</pre>
          <div className="mt-5 flex gap-2">
            <button onClick={() => this.setState({ error: null })} className="btn-ghost text-sm py-2">다시 시도</button>
            <Link to="/" className="btn-primary text-sm py-2">홈으로</Link>
          </div>
        </div>
      </div>
    )
  }
}
