import type { Session } from '../types'
import { isValidSessionSnapshot } from './sessionSnapshot'

const STORAGE_PREFIX = 'k8s-quiz:start-operation:v1:'
const MAX_PENDING_AGE_MS = 24 * 60 * 60 * 1000

export interface PendingStartOperation {
  userId: string
  problemId: string
  key: string
  createdAt: number
}

function storageKey(userId: string, problemId: string): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(userId)}:${encodeURIComponent(problemId)}`
}

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isPendingStartOperation(value: unknown): value is PendingStartOperation {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingStartOperation>
  return typeof pending.userId === 'string' && pending.userId.length > 0 &&
    typeof pending.problemId === 'string' && pending.problemId.length > 0 &&
    typeof pending.key === 'string' && /^start:[A-Za-z0-9-]{1,64}:[A-Za-z0-9-]{8,}$/.test(pending.key) &&
    typeof pending.createdAt === 'number' && Number.isFinite(pending.createdAt)
}

export function loadPendingStart(userId: string, problemId: string, now = Date.now()): PendingStartOperation | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(userId, problemId)
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isPendingStartOperation(value) || value.userId !== userId || value.problemId !== problemId ||
      value.createdAt > now || now - value.createdAt > MAX_PENDING_AGE_MS) {
      target.removeItem(key)
      return null
    }
    return value
  } catch {
    target.removeItem(key)
    return null
  }
}

export function getOrCreatePendingStart(userId: string, problemId: string): PendingStartOperation {
  const prior = loadPendingStart(userId, problemId)
  if (prior) return prior
  const pending: PendingStartOperation = {
    userId,
    problemId,
    key: `start:${problemId}:${crypto.randomUUID()}`,
    createdAt: Date.now(),
  }
  storage()?.setItem(storageKey(userId, problemId), JSON.stringify(pending))
  return pending
}

export function clearPendingStart(userId: string, problemId: string, expectedKey: string): void {
  const target = storage()
  if (!target) return
  const pending = loadPendingStart(userId, problemId)
  if (pending?.key === expectedKey) target.removeItem(storageKey(userId, problemId))
}

export function clearAllPendingStarts(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}

export function isValidStartResponse(value: unknown, problemId: string, requestId: string): value is Session {
  if (!isValidSessionSnapshot(value)) return false
  return value.request_id === requestId && value.problem_id === problemId
}
