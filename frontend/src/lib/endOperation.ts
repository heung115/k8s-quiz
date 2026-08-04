import { APIResponseError } from '../api/client'
import type { Session } from '../types'

const STORAGE_PREFIX = 'k8s-quiz:end-operation:v1:'
const MAX_PENDING_AGE_MS = 24 * 60 * 60 * 1000

export interface EndOperationScope {
  userId: string
  problemId: string
  sessionId: string
  generation: number
}

export interface PendingEndOperation extends EndOperationScope {
  key: string
  createdAt: number
}

export interface EndPendingResponse {
  request_id: string
  session_id: string
  generation: number
  status: 'cleanup_pending'
}

export type EndOperationResult =
  | { state: 'completed' }
  | { state: 'pending'; response: EndPendingResponse }

function scopeKey(scope: EndOperationScope): string {
  return [scope.userId, scope.problemId, scope.sessionId, String(scope.generation)].join('\0')
}

function storageKey(scope: EndOperationScope): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(scope.userId)}:${encodeURIComponent(scope.sessionId)}:${scope.generation}`
}

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isPendingEnd(value: unknown): value is PendingEndOperation {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingEndOperation>
  return typeof pending.userId === 'string' && pending.userId.length > 0 &&
    typeof pending.problemId === 'string' && pending.problemId.length > 0 &&
    typeof pending.sessionId === 'string' && pending.sessionId.length > 0 &&
    typeof pending.generation === 'number' && Number.isSafeInteger(pending.generation) && pending.generation > 0 &&
    typeof pending.key === 'string' && /^end:[A-Za-z0-9-]{8,}$/.test(pending.key) &&
    typeof pending.createdAt === 'number' && Number.isFinite(pending.createdAt)
}

export function loadPendingEnd(scope: EndOperationScope, now = Date.now()): PendingEndOperation | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(scope)
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isPendingEnd(value) || scopeKey(value) !== scopeKey(scope) || value.createdAt > now ||
      now - value.createdAt > MAX_PENDING_AGE_MS) {
      target.removeItem(key)
      return null
    }
    return value
  } catch {
    target.removeItem(key)
    return null
  }
}

export function loadPendingEndForProblem(
  userId: string,
  problemId: string,
  now = Date.now(),
): PendingEndOperation | null {
  const target = storage()
  if (!target) return null
  let selected: PendingEndOperation | null = null
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) {
    const raw = target.getItem(key)
    if (!raw) continue
    try {
      const value: unknown = JSON.parse(raw)
      if (!isPendingEnd(value) || key !== storageKey(value) || value.createdAt > now ||
        now - value.createdAt > MAX_PENDING_AGE_MS) {
        target.removeItem(key)
        continue
      }
      if (value.userId === userId && value.problemId === problemId &&
        (!selected || value.createdAt > selected.createdAt)) {
        selected = value
      }
    } catch {
      target.removeItem(key)
    }
  }
  return selected
}

export function getOrCreatePendingEnd(scope: EndOperationScope): PendingEndOperation {
  const prior = loadPendingEnd(scope)
  if (prior) return prior
  const pending: PendingEndOperation = {
    ...scope,
    key: `end:${crypto.randomUUID()}`,
    createdAt: Date.now(),
  }
  storage()?.setItem(storageKey(scope), JSON.stringify(pending))
  return pending
}

export function clearPendingEnd(scope: EndOperationScope, operationID: string): void {
  const target = storage()
  if (!target) return
  const pending = loadPendingEnd(scope)
  if (pending?.key === operationID) target.removeItem(storageKey(scope))
}

export function clearAllPendingEnds(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}

export function parseEndResponse(
  value: unknown,
  scope: EndOperationScope,
  operationID: string,
): EndOperationResult | null {
  if (value === undefined) return { state: 'completed' }
  if (!value || typeof value !== 'object') return null
  const response = value as Partial<EndPendingResponse>
  if (response.request_id !== operationID || response.session_id !== scope.sessionId ||
    response.generation !== scope.generation || response.status !== 'cleanup_pending') return null
  return { state: 'pending', response: response as EndPendingResponse }
}

export function canApplyEndResult(
  current: Session | null,
  scope: EndOperationScope,
  result: EndOperationResult,
  authoritativeNull = false,
): boolean {
  const exact = current?.problem_id === scope.problemId &&
    current.session_id === scope.sessionId && current.generation === scope.generation
  // A completed operation may race the authoritative null lifecycle frame.
  // It is safe to finish the page when no replacement session exists, but an
  // old response must never clear or mutate a newer/different allocation.
  return result.state === 'completed' ? exact || (current === null && authoritativeNull) : exact
}

export function isDefinitiveEndRejection(error: unknown): boolean {
  return error instanceof APIResponseError && [400, 401, 403, 404, 409].includes(error.status)
}

export class EndOperationController {
  private inFlight: { scope: string; promise: Promise<EndOperationResult> } | null = null

  async execute(
    scope: EndOperationScope,
    request: (operationID: string) => Promise<unknown>,
  ): Promise<EndOperationResult> {
    const currentScope = scopeKey(scope)
    if (this.inFlight?.scope === currentScope) return this.inFlight.promise

    const pending = getOrCreatePendingEnd(scope)
    const promise = request(pending.key).then((value) => {
      const result = parseEndResponse(value, scope, pending.key)
      if (!result) {
        throw new Error('서버가 올바른 종료 응답을 반환하지 않았습니다. 같은 요청으로 다시 시도합니다.')
      }
      if (result.state === 'completed') clearPendingEnd(scope, pending.key)
      return result
    }, (error: unknown) => {
      if (isDefinitiveEndRejection(error)) {
        clearPendingEnd(scope, pending.key)
      }
      throw error
    }).finally(() => {
      if (this.inFlight?.promise === promise) this.inFlight = null
    })
    this.inFlight = { scope: currentScope, promise }
    return promise
  }
}
