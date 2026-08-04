import { SESSION_STATUSES, type Session } from '../types'

const SESSION_STATUS_SET = new Set<string>(SESSION_STATUSES)

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function isPositiveInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
}

export function isValidSessionSnapshot(value: unknown): value is Session {
  if (!isRecord(value)) return false
  return (value.request_id === undefined ||
      (typeof value.request_id === 'string' && value.request_id.length > 0)) &&
    typeof value.operation_id === 'string' && value.operation_id.length > 0 &&
    typeof value.session_id === 'string' && value.session_id.length > 0 &&
    typeof value.problem_id === 'string' && value.problem_id.length > 0 &&
    isPositiveInteger(value.generation) &&
    typeof value.status === 'string' && SESSION_STATUS_SET.has(value.status) &&
    (value.timeout_at === null ||
      (typeof value.timeout_at === 'string' && Number.isFinite(Date.parse(value.timeout_at)))) &&
    typeof value.cleanup_pending === 'boolean' &&
    (value.terminal_reason === null || typeof value.terminal_reason === 'string') &&
    isPositiveInteger(value.event_sequence)
}

export function parseSessionSnapshot(value: unknown): Session | null {
  return isValidSessionSnapshot(value) ? value : null
}
