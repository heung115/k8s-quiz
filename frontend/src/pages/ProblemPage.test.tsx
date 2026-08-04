import { act } from 'react'
import { createRoot, Root } from 'react-dom/client'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { APIResponseError, api } from '../api/client'
import { getOrCreatePendingEnd, loadPendingEnd } from '../lib/endOperation'
import { useAuthStore } from '../stores/auth'
import { useSessionStore } from '../stores/session'
import type { Problem, Session, User } from '../types'
import { ProblemPage } from './ProblemPage'

Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
  value: true,
  configurable: true,
})

vi.mock('../hooks/useLifecycleStream', () => ({ useLifecycleStream: vi.fn() }))
vi.mock('../components/Terminal', () => ({ Terminal: () => <div>terminal</div> }))
vi.mock('../api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/client')>()
  return {
    ...actual,
    api: {
      get: vi.fn(),
      post: vi.fn(),
      put: vi.fn(),
      delete: vi.fn(),
    },
  }
})

const mockedGet = vi.mocked(api.get)
const mockedPost = vi.mocked(api.post)

const user: User = {
  id: 'u1',
  github_id: 1,
  username: 'tester',
  email: 'tester@example.test',
  avatar_url: '',
  role: 'user',
  created_at: '2030-01-01T00:00:00Z',
  updated_at: '2030-01-01T00:00:00Z',
}

const problem: Problem = {
  id: 'p1',
  title: 'Broken workload',
  description: 'Fix it',
  category: 'workload',
  difficulty: 'easy',
  type: 'fix',
  timeout_minutes: 30,
  verify_type: 'script',
  created_at: '2030-01-01T00:00:00Z',
}

const session: Session = {
  operation_id: 'start-operation',
  session_id: 's1',
  problem_id: 'p1',
  generation: 1,
  status: 'ready',
  timeout_at: null,
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 4,
}

let container: HTMLDivElement
let root: Root | null

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve()
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
}

async function renderPage(
  currentResult: () => Promise<unknown>,
  pageProblem: Problem = problem,
): Promise<void> {
  mockedGet.mockImplementation((path: string) => {
    if (path === '/api/problems/p1') return Promise.resolve(pageProblem) as Promise<never>
    if (path === '/api/sessions/current') return currentResult() as Promise<never>
    return Promise.reject(new Error(`unexpected GET ${path}`))
  })
  await act(async () => {
    root = createRoot(container)
    root.render(
      <MemoryRouter initialEntries={['/problems/p1']}>
        <Routes>
          <Route path="/problems/:id" element={<ProblemPage />} />
          <Route path="/" element={<div>dashboard</div>} />
        </Routes>
      </MemoryRouter>,
    )
  })
  await flush()
}

function buttonContaining(text: string): HTMLButtonElement {
  const button = Array.from(container.querySelectorAll('button')).find((candidate) =>
    candidate.textContent?.includes(text),
  )
  if (!button) throw new Error(`button not found: ${text}`)
  return button
}

beforeEach(() => {
  sessionStorage.clear()
  mockedGet.mockReset()
  mockedPost.mockReset()
  useAuthStore.setState({ user, bootstrapped: true })
  useSessionStore.setState({
    session: { ...session },
    stage: 'ready',
    stageMessage: '',
    crashed: false,
    verifyResult: null,
    wsConnected: false,
    lifecycleCursor: null,
    lifecycleConnected: false,
    lifecycleResyncing: false,
    lifecycleAuthoritative: false,
    authorityEpoch: 0,
    authoritativeNull: false,
    endIntent: null,
    notice: null,
  })
  getOrCreatePendingEnd({ userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 })
  container = document.createElement('div')
  document.body.appendChild(container)
  root = null
})

afterEach(async () => {
  if (root) {
    await act(async () => root?.unmount())
  }
  container.remove()
  vi.restoreAllMocks()
})

describe('ProblemPage — persisted durable end recovery', () => {
  it.each([
    ['HTTP 500', () => Promise.reject(new APIResponseError(500, 'unavailable'))],
    ['transport failure', () => Promise.reject(new TypeError('network failed'))],
    ['malformed 200', () => Promise.resolve({ ...session, cleanup_pending: undefined })],
  ])('does not replay end after an unconfirmed current read: %s', async (_name, currentResult) => {
    await renderPage(currentResult)

    expect(mockedPost).not.toHaveBeenCalledWith(
      '/api/sessions/end',
      expect.anything(),
      expect.anything(),
    )
    expect(loadPendingEnd({ userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 })).not.toBeNull()
  })

  it.each([
    ['strict current 200', () => Promise.resolve({ ...session })],
    ['exact current 404', () => Promise.reject(new APIResponseError(404, 'not found'))],
  ])('replays the exact persisted end after authoritative confirmation: %s', async (_name, currentResult) => {
    mockedPost.mockImplementation(async (_path, body, headers) => ({
      request_id: headers?.['Idempotency-Key'],
      session_id: (body as { session_id: string }).session_id,
      generation: (body as { generation: number }).generation,
      status: 'cleanup_pending',
    }))
    const pending = loadPendingEnd({ userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 })

    await renderPage(currentResult)

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost).toHaveBeenCalledWith(
      '/api/sessions/end',
      { session_id: 's1', generation: 1 },
      { 'Idempotency-Key': pending?.key },
    )
  })

  it('replays end when an authoritative lifecycle null wins during a failed current read', async () => {
    let rejectCurrent!: (reason: unknown) => void
    const current = new Promise<unknown>((_resolve, reject) => { rejectCurrent = reject })
    mockedPost.mockImplementation(async (_path, body, headers) => ({
      request_id: headers?.['Idempotency-Key'],
      session_id: (body as { session_id: string }).session_id,
      generation: (body as { generation: number }).generation,
      status: 'cleanup_pending',
    }))
    const pending = loadPendingEnd({ userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 })

    await renderPage(() => current)
    await act(async () => {
      useSessionStore.getState().handleLifecycleMessage({
        type: 'lifecycle_snapshot',
        schema: 'k8s-quiz.lifecycle/v1',
        session: null,
        cursor: null,
      })
      rejectCurrent(new APIResponseError(500, 'unavailable'))
    })
    await flush()

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost).toHaveBeenCalledWith(
      '/api/sessions/end',
      { session_id: 's1', generation: 1 },
      { 'Idempotency-Key': pending?.key },
    )
  })

  it('keeps a null-session cleanup overlay and retries only its exact persisted key', async () => {
    const pending = loadPendingEnd({ userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 })
    useSessionStore.setState({
      session: null,
      lifecycleAuthoritative: true,
      authoritativeNull: true,
      endIntent: { problemId: 'p1', sessionId: 's1', generation: 1 },
    })
    mockedPost.mockImplementation(async (_path, body, headers) => ({
      request_id: headers?.['Idempotency-Key'],
      session_id: (body as { session_id: string }).session_id,
      generation: (body as { generation: number }).generation,
      status: 'cleanup_pending',
    }))
    await renderPage(() => Promise.reject(new APIResponseError(500, 'unavailable')))

    const startButton = Array.from(container.querySelectorAll('button')).find((button) => button.textContent?.includes('환경 시작'))
    const statusButton = Array.from(container.querySelectorAll('button')).find((button) => button.textContent?.includes('정리 상태 확인'))
    expect(startButton?.disabled).toBe(true)
    expect(container.textContent).toContain('실행 환경을 정리하고 있습니다')
    expect(statusButton).toBeDefined()

    await act(async () => statusButton?.click())
    await flush()

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost).toHaveBeenCalledWith(
      '/api/sessions/end',
      { session_id: 's1', generation: 1 },
      { 'Idempotency-Key': pending?.key },
    )
  })

  it('settles a deferred 204 after lifecycle null and clears the exact end intent', async () => {
    let resolveEnd!: (value: unknown) => void
    mockedPost.mockImplementation((path) => {
      if (path !== '/api/sessions/end') throw new Error(`unexpected POST ${path}`)
      return new Promise((resolve) => { resolveEnd = resolve })
    })
    const exact = { userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 }

    await renderPage(() => Promise.resolve({ ...session }))
    expect(mockedPost).toHaveBeenCalledTimes(1)

    await act(async () => {
      useSessionStore.getState().handleLifecycleMessage({
        type: 'lifecycle_snapshot',
        schema: 'k8s-quiz.lifecycle/v1',
        session: null,
        cursor: null,
      })
      await Promise.resolve()
    })
    expect(useSessionStore.getState().endIntent).toMatchObject({ sessionId: 's1', generation: 1 })

    await act(async () => {
      resolveEnd(undefined)
      await Promise.resolve()
    })
    await flush()

    expect(container.textContent).toContain('dashboard')
    expect(useSessionStore.getState().endIntent).toBeNull()
    expect(loadPendingEnd(exact)).toBeNull()
  })

  it('clears a deferred 204 ledger and end intent after the page unmounts', async () => {
    let resolveEnd!: (value: unknown) => void
    mockedPost.mockImplementation((path) => {
      if (path !== '/api/sessions/end') throw new Error(`unexpected POST ${path}`)
      return new Promise((resolve) => { resolveEnd = resolve })
    })
    const exact = { userId: 'u1', problemId: 'p1', sessionId: 's1', generation: 1 }

    await renderPage(() => Promise.resolve({ ...session }))
    expect(mockedPost).toHaveBeenCalledTimes(1)
    await act(async () => {
      root?.unmount()
      root = null
    })
    expect(useSessionStore.getState().endIntent).toMatchObject({ sessionId: 's1', generation: 1 })

    await act(async () => {
      resolveEnd(undefined)
      await Promise.resolve()
    })

    expect(useSessionStore.getState().endIntent).toBeNull()
    expect(loadPendingEnd(exact)).toBeNull()
  })
})

describe('ProblemPage — synchronous mutation exclusion', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  it('sends one start when the start button is clicked twice in one act tick', async () => {
    useSessionStore.setState({ session: null, authoritativeNull: true })
    mockedPost.mockImplementation(() => new Promise(() => {}))
    await renderPage(() => Promise.reject(new APIResponseError(404, 'not found')))

    const start = buttonContaining('환경 시작')
    act(() => {
      start.click()
      start.click()
    })

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost.mock.calls[0]?.[0]).toBe('/api/problems/p1/start')
  })

  it('allows only reset when reset, verify, and end are clicked in one act tick', async () => {
    mockedPost.mockImplementation(() => new Promise(() => {}))
    await renderPage(() => Promise.resolve({ ...session }))

    const reset = buttonContaining('리셋')
    const verify = buttonContaining('검증')
    const end = buttonContaining('종료')
    act(() => {
      reset.click()
      verify.click()
      end.click()
    })

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost.mock.calls[0]?.[0]).toBe('/api/problems/p1/reset')
  })

  it('allows only end when end, verify, and reset are clicked in one act tick', async () => {
    mockedPost.mockImplementation(() => new Promise(() => {}))
    await renderPage(() => Promise.resolve({ ...session }))

    const end = buttonContaining('종료')
    const verify = buttonContaining('검증')
    const reset = buttonContaining('리셋')
    act(() => {
      end.click()
      verify.click()
      reset.click()
    })

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost.mock.calls[0]?.[0]).toBe('/api/sessions/end')
  })

  it('allows only choice submission when choice, end, and reset race in one act tick', async () => {
    const choiceProblem: Problem = {
      ...problem,
      verify_type: 'choice',
      choices: [{ id: 'a', text: 'First cause' }, { id: 'b', text: 'Second cause' }],
    }
    mockedPost.mockImplementation(() => new Promise(() => {}))
    await renderPage(() => Promise.resolve({ ...session }), choiceProblem)

    const choice = container.querySelector<HTMLInputElement>('input[value="a"]')
    if (!choice) throw new Error('choice input not found')
    act(() => choice.click())
    const submit = buttonContaining('답안 제출')
    const end = buttonContaining('종료')
    const reset = buttonContaining('리셋')
    act(() => {
      submit.click()
      end.click()
      reset.click()
    })

    expect(mockedPost).toHaveBeenCalledTimes(1)
    expect(mockedPost.mock.calls[0]?.[0]).toBe('/api/problems/p1/submit')
  })
})

describe('ProblemPage — late choice authority', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  it('does not apply a successful choice response after lifecycle makes the session null', async () => {
    const choiceProblem: Problem = {
      ...problem,
      verify_type: 'choice',
      choices: [{ id: 'a', text: 'First cause' }, { id: 'b', text: 'Second cause' }],
    }
    let resolveChoice!: (value: unknown) => void
    mockedPost.mockImplementation((_path, _body, headers) => new Promise((resolve) => {
      resolveChoice = () => resolve({
        request_id: headers?.['Idempotency-Key'],
        problem_id: 'p1',
        session_id: 's1',
        generation: 1,
        success: true,
      })
    }))
    await renderPage(() => Promise.resolve({ ...session }), choiceProblem)

    const choice = container.querySelector<HTMLInputElement>('input[value="a"]')
    if (!choice) throw new Error('choice input not found')
    act(() => choice.click())
    act(() => buttonContaining('답안 제출').click())
    expect(mockedPost).toHaveBeenCalledTimes(1)

    await act(async () => {
      useSessionStore.getState().handleLifecycleMessage({
        type: 'lifecycle_snapshot',
        schema: 'k8s-quiz.lifecycle/v1',
        session: null,
        cursor: null,
      })
      resolveChoice(undefined)
      await Promise.resolve()
    })
    await flush()

    expect(useSessionStore.getState().verifyResult).toBeNull()
    expect(container.textContent).not.toContain('VERIFICATION PASSED')
  })
})
