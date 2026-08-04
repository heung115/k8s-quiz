import type { Session } from '../types'
import { isValidSessionSnapshot } from './sessionSnapshot'

const STORAGE_PREFIX = 'k8s-quiz:reset-operation:v1:'
const MAX_PENDING_AGE_MS = 24 * 60 * 60 * 1000

export interface PendingResetOperation {
  userId: string
  problemId: string
  sessionId: string
  sourceGeneration: number
  key: string
  createdAt: number
  operationId?: string
  targetGeneration?: number
  acceptedAt?: number
}

function storageKey(userId: string, sessionId: string, sourceGeneration: number): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(userId)}:${encodeURIComponent(sessionId)}:${sourceGeneration}`
}

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isPendingResetOperation(value: unknown): value is PendingResetOperation {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingResetOperation>
  return typeof pending.userId === 'string' && pending.userId.length > 0 &&
    typeof pending.problemId === 'string' && pending.problemId.length > 0 &&
    typeof pending.sessionId === 'string' && pending.sessionId.length > 0 &&
    typeof pending.sourceGeneration === 'number' && Number.isSafeInteger(pending.sourceGeneration) && pending.sourceGeneration > 0 &&
    typeof pending.key === 'string' && /^reset:[A-Za-z0-9-]{1,64}:[A-Za-z0-9-]{8,}$/.test(pending.key) &&
    typeof pending.createdAt === 'number' && Number.isFinite(pending.createdAt) &&
    (pending.operationId === undefined || (typeof pending.operationId === 'string' && pending.operationId.length > 0)) &&
    (pending.targetGeneration === undefined || (typeof pending.targetGeneration === 'number' &&
      Number.isSafeInteger(pending.targetGeneration) && pending.targetGeneration === pending.sourceGeneration + 1)) &&
    (pending.acceptedAt === undefined || (typeof pending.acceptedAt === 'number' && Number.isFinite(pending.acceptedAt)))
}

function parsePending(target: Storage, key: string, now: number): PendingResetOperation | null {
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isPendingResetOperation(value) || value.createdAt > now || now - value.createdAt > MAX_PENDING_AGE_MS ||
      key !== storageKey(value.userId, value.sessionId, value.sourceGeneration)) {
      target.removeItem(key)
      return null
    }
    return value
  } catch {
    target.removeItem(key)
    return null
  }
}

export function loadPendingReset(
  userId: string,
  sessionId: string,
  sourceGeneration: number,
  now = Date.now(),
): PendingResetOperation | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(userId, sessionId, sourceGeneration)
  const pending = parsePending(target, key, now)
  if (!pending || pending.userId !== userId || pending.sessionId !== sessionId ||
    pending.sourceGeneration !== sourceGeneration) return null
  return pending
}

export function loadPendingResetForProblem(
  userId: string,
  problemId: string,
  now = Date.now(),
): PendingResetOperation | null {
  const target = storage()
  if (!target) return null
  let selected: PendingResetOperation | null = null
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) {
    const pending = parsePending(target, key, now)
    if (pending?.userId === userId && pending.problemId === problemId &&
      (!selected || pending.createdAt > selected.createdAt)) {
      selected = pending
    }
  }
  return selected
}

export function getOrCreatePendingReset(
  userId: string,
  problemId: string,
  sessionId: string,
  sourceGeneration: number,
): PendingResetOperation {
  const prior = loadPendingReset(userId, sessionId, sourceGeneration)
  if (prior) return prior
  const pending: PendingResetOperation = {
    userId,
    problemId,
    sessionId,
    sourceGeneration,
    key: `reset:${sourceGeneration}:${crypto.randomUUID()}`,
    createdAt: Date.now(),
  }
  storage()?.setItem(storageKey(userId, sessionId, sourceGeneration), JSON.stringify(pending))
  return pending
}

export function clearPendingReset(pending: PendingResetOperation): void {
  const target = storage()
  if (!target) return
  const key = storageKey(pending.userId, pending.sessionId, pending.sourceGeneration)
  const current = parsePending(target, key, Date.now())
  if (current?.key === pending.key) target.removeItem(key)
}

export function acknowledgePendingReset(
  pending: PendingResetOperation,
  snapshot: Session,
  now = Date.now(),
): PendingResetOperation | null {
  if (!isValidResetResponse(snapshot, pending) || !snapshot.operation_id) return null
  const target = storage()
  if (!target) return null
  const key = storageKey(pending.userId, pending.sessionId, pending.sourceGeneration)
  const current = parsePending(target, key, now)
  if (!current || current.key !== pending.key) return null
  const accepted: PendingResetOperation = {
    ...current,
    operationId: snapshot.operation_id,
    targetGeneration: snapshot.generation,
    acceptedAt: now,
  }
  target.setItem(key, JSON.stringify(accepted))
  return accepted
}

const RESET_CONVERGED_STATUSES = new Set([
  'provisioning',
  'booting',
  'setting_up',
  'ready',
  'completed',
  'failed',
  'timed_out',
  'provider_lost',
  'destroying',
  'destroyed',
])

export function clearPendingResetAfterLifecycle(
  userId: string,
  snapshot: Session,
): boolean {
  if (!snapshot.operation_id || snapshot.cleanup_pending !== false ||
    !RESET_CONVERGED_STATUSES.has(snapshot.status)) return false
  const pending = loadPendingReset(userId, snapshot.session_id, snapshot.generation - 1)
  if (!pending || pending.operationId !== snapshot.operation_id ||
    pending.targetGeneration !== snapshot.generation) return false
  clearPendingReset(pending)
  return true
}

export function clearAllPendingResets(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}

export function isValidResetResponse(value: unknown, pending: PendingResetOperation): value is Session {
  if (!isValidSessionSnapshot(value)) return false
  return value.request_id === pending.key &&
    value.session_id === pending.sessionId &&
    value.problem_id === pending.problemId &&
    value.generation === pending.sourceGeneration + 1
}
