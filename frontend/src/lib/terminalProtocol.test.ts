import { describe, expect, it, vi } from 'vitest'
import {
  canSendTerminalFrame,
  isExactTerminalAttached,
  parseTerminalServerMessage,
  shouldReconnectTerminal,
  terminalInputFrame,
  terminalReadyFrame,
  terminalResizeFrame,
} from './terminalProtocol'
import { WS_CLOSE_CONNECTION_REPLACED } from '../types'

describe('terminal attach protocol', () => {
  it('accepts only the exact identity and a valid matching attach nonce', () => {
    const exact = { type: 'terminal_attached' as const, session_id: 's1', generation: 2, attach_nonce: 'nonce-0001' }
    expect(isExactTerminalAttached(exact, 's1', 2)).toBe(true)
    expect(isExactTerminalAttached(exact, 's1', 2, 'nonce-0001')).toBe(true)
    expect(isExactTerminalAttached(exact, 's1', 2, 'nonce-0002')).toBe(false)
    expect(isExactTerminalAttached({ ...exact, session_id: 's2' }, 's1', 2)).toBe(false)
    expect(isExactTerminalAttached({ ...exact, generation: 3 }, 's1', 2)).toBe(false)
    expect(isExactTerminalAttached({ ...exact, attach_nonce: '' }, 's1', 2)).toBe(false)
    expect(isExactTerminalAttached({ ...exact, attach_nonce: 'bad nonce' }, 's1', 2)).toBe(false)
    expect(isExactTerminalAttached({ ...exact, attach_nonce: undefined } as never, 's1', 2)).toBe(false)
    expect(isExactTerminalAttached({ type: 'output', data: 'hello' }, 's1', 2)).toBe(false)
    expect(terminalReadyFrame('nonce-0001')).toEqual({ type: 'terminal_ready', attach_nonce: 'nonce-0001' })
    expect(terminalInputFrame('nonce-0001', 'ls\r')).toEqual({
      type: 'input', attach_nonce: 'nonce-0001', data: 'ls\r',
    })
    expect(terminalResizeFrame('nonce-0001', 120, 40)).toEqual({
      type: 'resize', attach_nonce: 'nonce-0001', cols: 120, rows: 40,
    })
  })

  it('blocks input before attach and from a stale socket', () => {
    vi.stubGlobal('WebSocket', { OPEN: 1 })
    const active = { readyState: 1 } as WebSocket
    const stale = { readyState: 1 } as WebSocket
    expect(canSendTerminalFrame(false, 'nonce-0001', active, active)).toBe(false)
    expect(canSendTerminalFrame(true, null, active, active)).toBe(false)
    expect(canSendTerminalFrame(true, 'nonce-0001', active, stale)).toBe(false)
    expect(canSendTerminalFrame(true, 'nonce-0001', active, active)).toBe(true)
    vi.unstubAllGlobals()
  })

  it('does not reconnect an intentionally replaced tab', () => {
    expect(shouldReconnectTerminal(WS_CLOSE_CONNECTION_REPLACED)).toBe(false)
    expect(shouldReconnectTerminal(1006)).toBe(true)
  })

  it('parses only known runtime frames with their required field types', () => {
    expect(parseTerminalServerMessage({ type: 'output', data: 'hello' })).toEqual({ type: 'output', data: 'hello' })
    expect(parseTerminalServerMessage({ type: 'error', message: 'failed' })).toEqual({ type: 'error', message: 'failed' })
    expect(parseTerminalServerMessage({
      type: 'terminal_attached', session_id: 's1', generation: 2, attach_nonce: 'nonce-1',
    })).toMatchObject({ type: 'terminal_attached', generation: 2 })
    expect(parseTerminalServerMessage({ type: 'output', data: 7 })).toBeNull()
    expect(parseTerminalServerMessage({ type: 'error', message: null })).toBeNull()
    expect(parseTerminalServerMessage({ type: 'terminal_attached', session_id: 's1', generation: 0, attach_nonce: 'n' })).toBeNull()
    expect(parseTerminalServerMessage({ type: 'unknown' })).toBeNull()
    expect(parseTerminalServerMessage(null)).toBeNull()
  })
})
