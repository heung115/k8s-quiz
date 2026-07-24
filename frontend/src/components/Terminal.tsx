import { useEffect, useRef, useState } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'
import { useAuthStore } from '../stores/auth'
import { useSessionStore } from '../stores/session'
import { useThemeStore } from '../stores/theme'
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
// attempts per close cycle; the counter resets once a socket opens again.
const MAX_RECONNECT_ATTEMPTS = 5
const MAX_BACKOFF_MS = 4000

export function Terminal() {
  const termRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const [reconnecting, setReconnecting] = useState(false)
  const { accessToken } = useAuthStore()
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

    const connect = () => {
      if (disposed) return
      const ws = new WebSocket(wsUrl)
      wsRef.current = ws

      ws.onopen = () => {
        if (disposed) { ws.close(); return }
        // Healthy open: reset the backoff counter for the next close cycle.
        attempts = 0
        setReconnecting(false)
        const authMsg: WSMessage = { type: 'auth', token: accessToken || '' }
        ws.send(JSON.stringify(authMsg))
        setWsConnected(true)
        term.writeln('\r\n\x1b[32mConnecting to terminal...\x1b[0m\r\n')
      }

      ws.onmessage = (event) => {
        try {
          const msg: WSMessage = JSON.parse(event.data)
          switch (msg.type) {
            case 'output':
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

      ws.onclose = () => {
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
          term.writeln('\r\n\x1b[31mDisconnected from terminal. Reload to reconnect.\x1b[0m\r\n')
          return
        }
        // Unexpected close mid-session (e.g. reset swapped the container):
        // re-attach with the same session via the auth handshake, with backoff.
        const delay = Math.min(1000 * 2 ** attempts, MAX_BACKOFF_MS)
        attempts += 1
        setReconnecting(true)
        reconnectTimer = window.setTimeout(connect, delay)
      }

      ws.onerror = () => {
        term.writeln('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n')
      }
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
  }, [accessToken])

  return (
    <div className="relative h-full w-full">
      <div ref={termRef} className="h-full w-full" />
      {reconnecting && (
        <div className="pointer-events-none absolute right-2 top-2 inline-flex items-center gap-1.5 border border-warning/40 bg-surface/90 px-2 py-1 micro text-warning motion-safe:animate-pulse" role="status">
          재연결 중…
        </div>
      )}
    </div>
  )
}
