import { useEffect, useRef } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'
import { useAuthStore } from '../stores/auth'
import { useSessionStore } from '../stores/session'
import { WSMessage } from '../types'

export function Terminal() {
  const termRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const { accessToken } = useAuthStore()
  const { setWsConnected, handleWSMessage } = useSessionStore()

  useEffect(() => {
    if (!termRef.current) return

    const term = new XTerm({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      theme: {
        background: '#12151c',
        foreground: '#e7eaf0',
        cursor: '#e7eaf0',
      },
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
    const ws = new WebSocket(wsUrl)
    wsRef.current = ws

    ws.onopen = () => {
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
      setWsConnected(false)
      term.writeln('\r\n\x1b[31mDisconnected from terminal.\x1b[0m\r\n')
    }

    ws.onerror = () => {
      term.writeln('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n')
    }

    term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data }))
      }
    })

    term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }))
      }
    })

    const handleResize = () => fitAddon.fit()
    window.addEventListener('resize', handleResize)

    return () => {
      window.removeEventListener('resize', handleResize)
      ws.close()
      term.dispose()
    }
  }, [accessToken])

  return <div ref={termRef} className="h-full w-full" />
}
