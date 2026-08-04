import { act } from 'react'
import { createRoot, Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ensureWebSocketAuth } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useSessionStore } from '../stores/session'
import type { Session, User } from '../types'
import { Terminal } from './Terminal'

Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
  value: true,
  configurable: true,
})

const xtermState = vi.hoisted(() => ({
  writes: [] as string[],
  sends: [] as string[],
  fit: vi.fn(),
  dispose: vi.fn(),
}))

vi.mock('@xterm/xterm', () => ({
  Terminal: class MockTerminal {
    options: Record<string, unknown> = {}
    loadAddon() {}
    open() {}
    write(value: string) { xtermState.writes.push(value) }
    writeln() {}
    onData() { return { dispose: vi.fn() } }
    onResize() { return { dispose: vi.fn() } }
    dispose() { xtermState.dispose() }
  },
}))

vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class MockFitAddon {
    fit() { xtermState.fit() }
  },
}))

vi.mock('@xterm/addon-web-links', () => ({
  WebLinksAddon: class MockWebLinksAddon {},
}))

vi.mock('../api/client', () => ({
  ensureWebSocketAuth: vi.fn(async () => true),
}))

class FakeWebSocket {
  static readonly OPEN = 1
  static readonly instances: FakeWebSocket[] = []

  readonly url: string
  readyState = FakeWebSocket.OPEN
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent<string>) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  closes: Array<{ code: number; reason: string }> = []

  constructor(url: string | URL) {
    this.url = String(url)
    FakeWebSocket.instances.push(this)
  }

  send(value: string) {
    xtermState.sends.push(value)
  }

  close(code = 1000, reason = '') {
    this.closes.push({ code, reason })
    this.readyState = 3
    this.onclose?.({ code } as CloseEvent)
  }

  serverOpen() {
    this.onopen?.(new Event('open'))
  }

  serverMessage(value: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(value) }))
  }

  serverClose(code: number) {
    this.readyState = 3
    this.onclose?.({ code } as CloseEvent)
  }
}

const user = { id: 'user-1' } as User
const session: Session = {
  operation_id: 'operation-1',
  session_id: 'session-1',
  problem_id: 'problem-1',
  generation: 3,
  status: 'ready',
  timeout_at: null,
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 4,
}

let container: HTMLDivElement
let root: Root | null

async function flushMicrotasks(): Promise<void> {
  await act(async () => {
    await Promise.resolve()
    await Promise.resolve()
  })
}

async function renderTerminal(): Promise<void> {
  await act(async () => {
    root = createRoot(container)
    root.render(<Terminal sessionId={session.session_id} generation={session.generation} />)
  })
  await flushMicrotasks()
}

describe('Terminal reconnect budget', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.stubGlobal('WebSocket', FakeWebSocket)
    FakeWebSocket.instances.length = 0
    xtermState.writes.length = 0
    xtermState.sends.length = 0
    xtermState.fit.mockClear()
    xtermState.dispose.mockClear()
    vi.mocked(ensureWebSocketAuth).mockResolvedValue(true)
    useAuthStore.setState({ user, bootstrapped: true })
    useSessionStore.setState({
      session: { ...session },
      crashed: false,
      wsConnected: false,
    })
    container = document.createElement('div')
    document.body.appendChild(container)
    root = null
  })

  afterEach(async () => {
    if (root) {
      await act(async () => root?.unmount())
    }
    container.remove()
    vi.clearAllTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('stops after initial plus five ACK-only connections', async () => {
    await renderTerminal()

    const retryDelays = [1000, 2000, 4000, 4000, 4000]
    for (let index = 0; index < 6; index += 1) {
      const socket = FakeWebSocket.instances[index]
      expect(socket).toBeDefined()
      expect(socket.url).toContain('session_id=session-1')
      expect(socket.url).toContain('generation=3')

      act(() => {
        socket.serverOpen()
        socket.serverMessage({
          type: 'terminal_attached',
          session_id: session.session_id,
          generation: session.generation,
          attach_nonce: `nonce-${index + 1}`,
        })
      })
      expect(JSON.parse(xtermState.sends[xtermState.sends.length - 1] || '{}')).toEqual({
        type: 'terminal_ready',
        attach_nonce: `nonce-${index + 1}`,
      })

      act(() => socket.serverClose(1006))
      if (index < retryDelays.length) {
        await act(async () => {
          vi.advanceTimersByTime(retryDelays[index])
          await Promise.resolve()
        })
        await flushMicrotasks()
      }
    }

    expect(FakeWebSocket.instances).toHaveLength(6)
    expect(container.querySelector('[role="alert"]')?.textContent).toContain('터미널 연결 실패')
    await act(async () => {
      vi.advanceTimersByTime(60_000)
      await Promise.resolve()
    })
    expect(FakeWebSocket.instances).toHaveLength(6)
    expect(xtermState.writes).toEqual([])
  })

  it('never reconnects a socket closed because another tab owns it', async () => {
    await renderTerminal()
    act(() => FakeWebSocket.instances[0].serverClose(4001))
    await act(async () => {
      vi.advanceTimersByTime(60_000)
      await Promise.resolve()
    })
    expect(FakeWebSocket.instances).toHaveLength(1)
    expect(container.querySelector('[role="alert"]')).toBeNull()
  })

  it('sends ready once and rejects a duplicate attach on the same socket', async () => {
    await renderTerminal()
    const socket = FakeWebSocket.instances[0]
    const attached = {
      type: 'terminal_attached',
      session_id: session.session_id,
      generation: session.generation,
      attach_nonce: 'nonce-once',
    }

    act(() => {
      socket.serverMessage(attached)
      socket.serverMessage(attached)
    })

    expect(xtermState.sends.map((value) => JSON.parse(value))).toEqual([
      { type: 'terminal_ready', attach_nonce: 'nonce-once' },
    ])
    expect(socket.closes).toContainEqual({ code: 1008, reason: 'terminal_attach_replay' })
  })

  it('rejects an attach nonce reused by a replacement connection', async () => {
    await renderTerminal()
    const first = FakeWebSocket.instances[0]
    const attached = {
      type: 'terminal_attached',
      session_id: session.session_id,
      generation: session.generation,
      attach_nonce: 'nonce-reused',
    }
    act(() => {
      first.serverMessage(attached)
      first.serverClose(1006)
    })
    await act(async () => {
      vi.advanceTimersByTime(1000)
      await Promise.resolve()
    })
    await flushMicrotasks()

    const replacement = FakeWebSocket.instances[1]
    act(() => replacement.serverMessage(attached))

    expect(xtermState.sends.map((value) => JSON.parse(value))).toEqual([
      { type: 'terminal_ready', attach_nonce: 'nonce-reused' },
    ])
    expect(replacement.closes).toContainEqual({ code: 1008, reason: 'terminal_attach_replay' })
  })

  it.each([
    { type: 'output', data: 7 },
    { type: 'error', message: null },
    { type: 'unexpected' },
  ])('rejects a malformed or unknown runtime frame: $type', async (frame) => {
    await renderTerminal()
    const socket = FakeWebSocket.instances[0]
    act(() => socket.serverMessage(frame))
    expect(socket.closes).toContainEqual({ code: 1008, reason: 'terminal_protocol_violation' })
  })
})
