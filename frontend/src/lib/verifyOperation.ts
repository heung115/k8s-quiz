import { APIResponseError } from '../api/client'

const STORAGE_PREFIX = 'k8s-quiz:verify-operation:v1:'
const MAX_PENDING_AGE_MS = 24 * 60 * 60 * 1000

export interface VerifyOperationScope {
  userId: string
  problemId: string
  sessionId: string
  generation: number
}

export interface PendingVerifyOperation extends VerifyOperationScope {
  key: string
  createdAt: number
}

export interface VerifyOperationResponse {
  success: boolean
  log: string
}

function scopeKey(scope: VerifyOperationScope): string {
  return [scope.userId, scope.problemId, scope.sessionId, String(scope.generation)].join('\0')
}

function storageKey(scope: VerifyOperationScope): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(scope.userId)}:${encodeURIComponent(scope.problemId)}:${encodeURIComponent(scope.sessionId)}:${scope.generation}`
}

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isPendingVerify(value: unknown): value is PendingVerifyOperation {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingVerifyOperation>
  return typeof pending.userId === 'string' && pending.userId.length > 0 &&
    typeof pending.problemId === 'string' && pending.problemId.length > 0 &&
    typeof pending.sessionId === 'string' && pending.sessionId.length > 0 &&
    typeof pending.generation === 'number' && Number.isSafeInteger(pending.generation) && pending.generation > 0 &&
    typeof pending.key === 'string' && /^verify:[A-Za-z0-9-]{8,}$/.test(pending.key) &&
    typeof pending.createdAt === 'number' && Number.isFinite(pending.createdAt)
}

export function loadPendingVerify(scope: VerifyOperationScope, now = Date.now()): PendingVerifyOperation | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(scope)
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isPendingVerify(value) || scopeKey(value) !== scopeKey(scope) || value.createdAt > now ||
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

export function getOrCreatePendingVerify(scope: VerifyOperationScope): PendingVerifyOperation {
  const prior = loadPendingVerify(scope)
  if (prior) return prior
  const pending: PendingVerifyOperation = {
    ...scope,
    key: `verify:${crypto.randomUUID()}`,
    createdAt: Date.now(),
  }
  storage()?.setItem(storageKey(scope), JSON.stringify(pending))
  return pending
}

export function clearPendingVerify(scope: VerifyOperationScope, expectedKey: string): void {
  const target = storage()
  if (!target) return
  const pending = loadPendingVerify(scope)
  if (pending?.key === expectedKey) target.removeItem(storageKey(scope))
}

export function clearAllPendingVerifies(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}

export function isValidVerifyResponse(value: unknown): value is VerifyOperationResponse {
  if (!value || typeof value !== 'object') return false
  const response = value as Partial<VerifyOperationResponse>
  return typeof response.success === 'boolean' && typeof response.log === 'string'
}

export class VerifyOperationController {
  private inFlight: { scope: string; promise: Promise<VerifyOperationResponse> } | null = null

  // Reset only component-local request sharing. Ambiguous durable operations
  // intentionally survive component recreation and page reloads.
  reset(): void {
    this.inFlight = null
  }

  async execute(
    scope: VerifyOperationScope,
    request: (operationID: string) => Promise<unknown>,
  ): Promise<VerifyOperationResponse> {
    const currentScope = scopeKey(scope)
    if (this.inFlight?.scope === currentScope) return this.inFlight.promise

    const pending = getOrCreatePendingVerify(scope)
    const promise = request(pending.key).then((value) => {
      if (!isValidVerifyResponse(value)) {
        throw new Error('서버가 올바른 검증 응답을 반환하지 않았습니다. 같은 요청으로 다시 시도합니다.')
      }
      clearPendingVerify(scope, pending.key)
      return value
    }, (error: unknown) => {
      // A concrete HTTP response is terminal. Transport and malformed-2xx
      // outcomes remain ambiguous and retain the exact durable key.
      if (error instanceof APIResponseError) clearPendingVerify(scope, pending.key)
      throw error
    }).finally(() => {
      if (this.inFlight?.promise === promise) this.inFlight = null
    })
    this.inFlight = { scope: currentScope, promise }
    return promise
  }
}
