import { Link } from 'react-router-dom'

export function NotFound() {
  return (
    <div className="min-h-[70vh] flex flex-col items-center justify-center px-5 text-center">
      <p className="font-display font-bold text-6xl text-edge select-none">404</p>
      <p className="mt-3 font-mono text-sm text-ink-muted">경로를 찾을 수 없습니다</p>
      <Link to="/" className="btn-ghost text-sm py-2 mt-6">홈으로 돌아가기</Link>
    </div>
  )
}
