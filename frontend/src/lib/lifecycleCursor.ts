import type { LifecycleCursor } from '../types'

const STORAGE_PREFIX = 'k8s-quiz:lifecycle-cursor:v1:'

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function storageKey(userId: string, sessionId: string, generation: number): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(userId)}:${encodeURIComponent(sessionId)}:${generation}`
}

function isCursor(value: unknown): value is LifecycleCursor {
  if (!value || typeof value !== 'object') return false
  const cursor = value as Partial<LifecycleCursor>
  return typeof cursor.session_id === 'string' && cursor.session_id.length > 0 &&
    typeof cursor.generation === 'number' && Number.isSafeInteger(cursor.generation) && cursor.generation > 0 &&
    typeof cursor.event_sequence === 'number' && Number.isSafeInteger(cursor.event_sequence) && cursor.event_sequence > 0
}

export function loadLifecycleCursor(
  userId: string,
  sessionId: string,
  generation: number,
): LifecycleCursor | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(userId, sessionId, generation)
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isCursor(value) || value.session_id !== sessionId || value.generation !== generation) {
      target.removeItem(key)
      return null
    }
    return value
  } catch {
    target.removeItem(key)
    return null
  }
}

export function saveLifecycleCursor(userId: string, cursor: LifecycleCursor): void {
  storage()?.setItem(
    storageKey(userId, cursor.session_id, cursor.generation),
    JSON.stringify(cursor),
  )
}

export function clearLifecycleCursor(userId: string, cursor: LifecycleCursor): void {
  storage()?.removeItem(storageKey(userId, cursor.session_id, cursor.generation))
}

export function clearAllLifecycleCursors(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}
