import {
  type TerminalInputMessage,
  type TerminalAttachedMessage,
  type TerminalReadyMessage,
  type TerminalResizeMessage,
  type TerminalServerMessage,
  WS_CLOSE_CONNECTION_REPLACED,
} from '../types'

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

export function parseTerminalServerMessage(value: unknown): TerminalServerMessage | null {
  if (!isRecord(value) || typeof value.type !== 'string') return null
  switch (value.type) {
    case 'terminal_attached':
      if (typeof value.session_id !== 'string' || value.session_id.length === 0 ||
        typeof value.generation !== 'number' || !Number.isSafeInteger(value.generation) || value.generation <= 0 ||
        typeof value.attach_nonce !== 'string') return null
      return value as unknown as TerminalAttachedMessage
    case 'output':
      return typeof value.data === 'string' ? { type: 'output', data: value.data } : null
    case 'error':
      return typeof value.message === 'string' ? { type: 'error', message: value.message } : null
    default:
      return null
  }
}

export function isExactTerminalAttached(
  message: TerminalServerMessage,
  sessionId: string,
  generation: number,
  expectedNonce?: string,
): message is TerminalAttachedMessage {
  return message.type === 'terminal_attached' &&
    message.session_id === sessionId && message.generation === generation &&
    typeof message.attach_nonce === 'string' && /^[^\s\u0000-\u001f\u007f]{1,512}$/u.test(message.attach_nonce) &&
    (expectedNonce === undefined || message.attach_nonce === expectedNonce)
}

export function terminalReadyFrame(attachNonce: string): TerminalReadyMessage {
  return { type: 'terminal_ready', attach_nonce: attachNonce }
}

export function terminalInputFrame(attachNonce: string, data: string): TerminalInputMessage {
  return { type: 'input', attach_nonce: attachNonce, data }
}

export function terminalResizeFrame(attachNonce: string, cols: number, rows: number): TerminalResizeMessage {
  return { type: 'resize', attach_nonce: attachNonce, cols, rows }
}

export function shouldReconnectTerminal(closeCode: number): boolean {
  return closeCode !== WS_CLOSE_CONNECTION_REPLACED
}

export function canSendTerminalFrame(
  attached: boolean,
  attachNonce: string | null,
  active: WebSocket | null,
  candidate: WebSocket | null,
): candidate is WebSocket {
  return attached && attachNonce !== null && candidate !== null && candidate === active &&
    candidate.readyState === WebSocket.OPEN
}
