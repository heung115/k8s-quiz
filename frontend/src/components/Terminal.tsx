import { useEffect, useRef, useState } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'
import { useSessionStore } from '../stores/session'
import { useThemeStore } from '../stores/theme'
import { TerminalServerMessage } from '../types'
import { ensureWebSocketAuth } from '../api/client'
import { useAuthStore } from '../stores/auth'
import {
  canSendTerminalFrame,
  isExactTerminalAttached,
  parseTerminalServerMessage,
  shouldReconnectTerminal,
  terminalInputFrame,
  terminalReadyFrame,
  terminalResizeFrame,
} from '../lib/terminalProtocol'

function readTermTheme() {
  const cs = getComputedStyle(document.documentElement)
  const ch = (n: string) => cs.getPropertyValue(n).trim()
  const rgb = (n: string) => { const c = ch(n); return c ? `rgb(${c})` : undefined }
  return {
    background: rgb('--terminal') || '#05080d',
    foreground: rgb('--ink') || '#e6edf6',
    cursor: rgb('--accent-hover') || '#4d84f0',
    selectionBackground: ch('--accent-soft') || 'rgba(50, 108, 229, 0.35)',
  }
}

// Reconnect policy: exponential backoff 1s→2s→4s (capped), at most this many
// failed attempts per close cycle. A connection that streams terminal output is
// healthy and resets the counter; a connection that closes without ever
// producing output (e.g. an expired access cookie the server can only reject
// after the upgrade) counts as a failed attempt instead of resetting it, so an
// idle page with a dead cookie can't loop forever.
const MAX_RECONNECT_ATTEMPTS = 5
const MAX_BACKOFF_MS = 4000

type TerminalProps = {
  sessionId: string
  generation: number
}

export function Terminal({ sessionId, generation }: TerminalProps) {
  const termRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const retryRef = useRef<() => void>(() => {})
  const [reconnecting, setReconnecting] = useState(false)
  const [failed, setFailed] = useState(false)
  const setWsConnected = useSessionStore((state) => state.setWsConnected)
  const theme = useThemeStore((s) => s.theme)

  useEffect(() => {
    if (xtermRef.current) {
      xtermRef.current.options.theme = readTermTheme()
    }
  }, [theme])

  useEffect(() => {
    if (!termRef.current) return

    const term = new XTerm({
      cursorBlink: true,
      fontSize: 13.5,
      fontFamily: '"IBM Plex Mono", Menlo, Monaco, monospace',
      theme: readTermTheme(),
    })

    const fitAddon = new FitAddon()
    const webLinksAddon = new WebLinksAddon()
    term.loadAddon(fitAddon)
    term.loadAddon(webLinksAddon)
    term.open(termRef.current)
    fitAddon.fit()

    xtermRef.current = term
    fitAddonRef.current = fitAddon

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
	    const identity = new URLSearchParams({ session_id: sessionId, generation: String(generation) })
	    const wsUrl = `${protocol}//${window.location.host}/ws/terminal?${identity.toString()}`

    let disposed = false
    let reconnectTimer: number | undefined
    let attempts = 0
    let attached = false
    let attachNonce: string | null = null
    let activeSocket: WebSocket | null = null
    const seenAttachNonces = new Set<string>()

    const connect = async () => {
      if (disposed) return
      attached = false
      attachNonce = null
      setWsConnected(false)
      if (!await ensureWebSocketAuth()) {
        if (!disposed && useAuthStore.getState().user) {
          scheduleReconnect()
        }
        return
      }
      if (disposed) return
      const ws = new WebSocket(wsUrl)
      wsRef.current = ws
      activeSocket = ws
      const isCurrentSocket = () => !disposed && activeSocket === ws

      ws.onopen = () => {
        if (!isCurrentSocket()) { ws.close(); return }
        setReconnecting(false)
        // httpOnly cookies authenticate the upgrade; no auth message needed.
        term.writeln('\r\n\x1b[32mConnecting to terminal...\x1b[0m\r\n')
      }

      ws.onmessage = (event) => {
        if (!isCurrentSocket()) return
        try {
          const msg: TerminalServerMessage | null = parseTerminalServerMessage(JSON.parse(event.data))
          if (!msg) {
            ws.close(1008, 'terminal_protocol_violation')
            return
          }
          switch (msg.type) {
            case 'terminal_attached':
              if (attached || seenAttachNonces.has(msg.attach_nonce)) {
                ws.close(1008, 'terminal_attach_replay')
                return
              }
              if (!isExactTerminalAttached(msg, sessionId, generation)) {
                ws.close(1008, 'terminal_identity_mismatch')
                return
              }
              seenAttachNonces.add(msg.attach_nonce)
              attachNonce = msg.attach_nonce
              try {
                ws.send(JSON.stringify(terminalReadyFrame(attachNonce)))
              } catch {
                attachNonce = null
                ws.close(1011, 'terminal_ready_failed')
                return
              }
              // Local input opens only after the exact nonce response has been
              // synchronously handed to the active WebSocket.
              attached = true
              setWsConnected(true)
              break
            case 'output':
              if (!attached) return
              if (msg.data) {
                // An attach ACK proves only the pre-commit handshake. Count
                // this connection healthy after actual terminal output so an
                // ACK→commit-failure→EOF loop cannot reset the retry budget.
                attempts = 0
                setFailed(false)
                term.write(msg.data)
              }
              break
            case 'error':
              term.writeln(`\r\n\x1b[31mError: ${msg.message}\x1b[0m\r\n`)
              break
          }
        } catch {
          ws.close(1008, 'terminal_protocol_violation')
        }
      }

      ws.onclose = (event) => {
        if (!isCurrentSocket()) return
        activeSocket = null
        setWsConnected(false)
        if (!shouldReconnectTerminal(event.code)) {
          setReconnecting(false)
          setFailed(false)
          term.writeln('\r\n\x1b[33mTerminal connection moved to another tab.\x1b[0m\r\n')
          return
        }
        // Stop retrying once the session is clearly over: a non-crash
        // session_ended clears the session upstream (ProblemPage unmounts us),
        // and a crash surfaces the reset banner — neither should reconnect.
        const { session, crashed } = useSessionStore.getState()
        if (!session || crashed) {
          setReconnecting(false)
          term.writeln('\r\n\x1b[31mDisconnected from terminal.\x1b[0m\r\n')
          return
        }
        scheduleReconnect()
      }

      ws.onerror = () => {
        if (!isCurrentSocket()) return
        term.writeln('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n')
      }
    }

    const scheduleReconnect = () => {
      if (disposed || attempts >= MAX_RECONNECT_ATTEMPTS) {
        if (!disposed) {
          setReconnecting(false)
          setFailed(true)
        }
        return
      }
      const delay = Math.min(1000 * 2 ** attempts, MAX_BACKOFF_MS)
      attempts += 1
      setReconnecting(true)
      reconnectTimer = window.setTimeout(() => { void connect() }, delay)
    }

    retryRef.current = () => {
      if (disposed) return
      attempts = 0
      setFailed(false)
      setReconnecting(true)
      void connect()
    }

    void connect()

    term.onData((data) => {
      const ws = wsRef.current
      const nonce = attachNonce
      if (canSendTerminalFrame(attached, nonce, activeSocket, ws) && nonce !== null) {
        ws.send(JSON.stringify(terminalInputFrame(nonce, data)))
      }
    })

    term.onResize(({ cols, rows }) => {
      const ws = wsRef.current
      const nonce = attachNonce
      if (canSendTerminalFrame(attached, nonce, activeSocket, ws) && nonce !== null) {
        ws.send(JSON.stringify(terminalResizeFrame(nonce, cols, rows)))
      }
    })

    const handleResize = () => fitAddon.fit()
    window.addEventListener('resize', handleResize)

    return () => {
      disposed = true
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer)
      window.removeEventListener('resize', handleResize)
      const socket = activeSocket
      activeSocket = null
      wsRef.current = null
      setWsConnected(false)
      socket?.close(1000, 'terminal_unmounted')
      term.dispose()
    }
	  }, [generation, sessionId, setWsConnected])

  return (
    <div className="relative h-full w-full">
      <div ref={termRef} className="h-full w-full" />
      {reconnecting && (
        <div className="pointer-events-none absolute right-2 top-2 inline-flex items-center gap-1.5 border border-warning/40 bg-surface/90 px-2 py-1 micro text-warning motion-safe:animate-pulse" role="status">
          재연결 중…
        </div>
      )}
      {failed && (
        <div className="absolute inset-0 flex items-center justify-center bg-terminal/80 px-4" role="alert">
          <div className="w-full max-w-xs border border-danger/40 bg-surface px-5 py-4 text-center">
            <p className="font-display font-semibold text-sm text-danger">터미널 연결 실패</p>
            <p className="micro text-ink-faint mt-1.5">터미널 연결이 끊어졌습니다. lifecycle 상태는 별도로 계속 동기화됩니다.</p>
            <button
              onClick={() => retryRef.current()}
              className="mt-4 inline-flex items-center justify-center gap-1.5 border border-edge bg-surface-2 hover:bg-surface-3 px-3 py-1.5 text-sm transition-colors"
            >
              다시 시도
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
