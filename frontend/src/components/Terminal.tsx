import { useEffect, useRef, useState } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'
import { useSessionStore } from '../stores/session'
import { useThemeStore } from '../stores/theme'
import { refreshAuth } from '../api/client'
import { WSMessage } from '../types'

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

export function Terminal() {
  const termRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const retryRef = useRef<() => void>(() => {})
  const [reconnecting, setReconnecting] = useState(false)
  const [failed, setFailed] = useState(false)
  const { setWsConnected, handleWSMessage } = useSessionStore()
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
    const wsUrl = `${protocol}//${window.location.host}/ws/terminal`

    let disposed = false
    let reconnectTimer: number | undefined
    let attempts = 0
    let gotOutput = false

    const connect = () => {
      if (disposed) return
      gotOutput = false
      const ws = new WebSocket(wsUrl)
      wsRef.current = ws

      ws.onopen = () => {
        if (disposed) { ws.close(); return }
        setReconnecting(false)
        // httpOnly cookies authenticate the upgrade; no auth message needed.
        setWsConnected(true)
        term.writeln('\r\n\x1b[32mConnecting to terminal...\x1b[0m\r\n')
      }

      ws.onmessage = (event) => {
        try {
          const msg: WSMessage = JSON.parse(event.data)
          switch (msg.type) {
            case 'output':
              // First real output proves an authenticated, attached shell:
              // treat the connection as healthy and reset the backoff counter.
              if (!gotOutput) {
                gotOutput = true
                attempts = 0
                setFailed(false)
              }
              term.write(msg.data || '')
              break
            case 'stage':
            case 'verify_result':
            case 'session_ended':
            case 'timeout_warning':
              handleWSMessage(msg)
              if (msg.type === 'stage') {
                term.writeln(`\r\n\x1b[36m[${msg.stage}] ${msg.message}\x1b[0m\r\n`)
              } else if (msg.type === 'verify_result') {
                const color = msg.success ? '32' : '31'
                const text = msg.success ? '✓ Verification passed!' : '✗ Verification failed'
                term.writeln(`\r\n\x1b[${color}m${text}\x1b[0m\r\n`)
              } else if (msg.type === 'session_ended') {
                term.writeln(`\r\n\x1b[33mSession ended: ${msg.reason}\x1b[0m\r\n`)
              } else if (msg.type === 'timeout_warning') {
                term.writeln(`\r\n\x1b[33m⚠ ${msg.remaining_seconds}s remaining!\x1b[0m\r\n`)
              }
              break
            case 'error':
              term.writeln(`\r\n\x1b[31mError: ${msg.message}\x1b[0m\r\n`)
              break
          }
        } catch {}
      }

      ws.onclose = async () => {
        if (disposed) return
        setWsConnected(false)
        // Stop retrying once the session is clearly over: a non-crash
        // session_ended clears the session upstream (ProblemPage unmounts us),
        // and a crash surfaces the reset banner — neither should reconnect.
        const { session, crashed } = useSessionStore.getState()
        if (!session || crashed) {
          setReconnecting(false)
          term.writeln('\r\n\x1b[31mDisconnected from terminal.\x1b[0m\r\n')
          return
        }
        if (attempts >= MAX_RECONNECT_ATTEMPTS) {
          setReconnecting(false)
          setFailed(true)
          term.writeln('\r\n\x1b[31mTerminal connection failed. Please sign in again.\x1b[0m\r\n')
          return
        }
        // A no-output close is the signature of a stale access cookie: refresh
        // it (single-flight, shared with the REST client) before re-attaching.
        if (!gotOutput) {
          await refreshAuth()
          if (disposed) return
        }
        // Unexpected close mid-session (e.g. reset swapped the container):
        // re-attach with the same session — httpOnly cookies ride the new
        // upgrade, no auth handshake — with exponential backoff.
        const delay = Math.min(1000 * 2 ** attempts, MAX_BACKOFF_MS)
        attempts += 1
        setReconnecting(true)
        reconnectTimer = window.setTimeout(connect, delay)
      }

      ws.onerror = () => {
        term.writeln('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n')
      }
    }

    retryRef.current = () => {
      if (disposed) return
      attempts = 0
      setFailed(false)
      setReconnecting(true)
      connect()
    }

    connect()

    term.onData((data) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data }))
      }
    })

    term.onResize(({ cols, rows }) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }))
      }
    })

    const handleResize = () => fitAddon.fit()
    window.addEventListener('resize', handleResize)

    return () => {
      disposed = true
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer)
      window.removeEventListener('resize', handleResize)
      wsRef.current?.close()
      term.dispose()
    }
  }, [])

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
            <p className="micro text-ink-faint mt-1.5">세션이 만료되었을 수 있습니다. 다시 로그인하세요.</p>
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
