import { APIResponseError } from '../api/client'
import type { Session } from '../types'

const STORAGE_PREFIX = 'k8s-quiz:choice-operation:v1:'
const MAX_PENDING_AGE_MS = 24 * 60 * 60 * 1000

export interface ChoiceOperationScope {
  userId: string
  problemId: string
  sessionId: string
  generation: number
  choiceId: string
}

export interface PendingChoiceOperation extends ChoiceOperationScope {
  key: string
  createdAt: number
}

export interface ChoiceOperationResponse {
  request_id: string
  problem_id: string
  session_id: string
  generation: number
  success: boolean
}

function scopeKey(scope: ChoiceOperationScope): string {
  return [scope.userId, scope.problemId, scope.sessionId, String(scope.generation), scope.choiceId].join('\0')
}

function storageKey(scope: ChoiceOperationScope): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(scope.userId)}:${encodeURIComponent(scope.problemId)}:${encodeURIComponent(scope.sessionId)}:${scope.generation}:${encodeURIComponent(scope.choiceId)}`
}

function storage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage
  } catch {
    return null
  }
}

function isPendingChoice(value: unknown): value is PendingChoiceOperation {
  if (!value || typeof value !== 'object') return false
  const pending = value as Partial<PendingChoiceOperation>
  return typeof pending.userId === 'string' && pending.userId.length > 0 &&
    typeof pending.problemId === 'string' && pending.problemId.length > 0 &&
    typeof pending.sessionId === 'string' && pending.sessionId.length > 0 &&
    typeof pending.generation === 'number' && Number.isSafeInteger(pending.generation) && pending.generation > 0 &&
    typeof pending.choiceId === 'string' && pending.choiceId.length > 0 &&
    typeof pending.key === 'string' && /^choice:[A-Za-z0-9-]{8,}$/.test(pending.key) &&
    typeof pending.createdAt === 'number' && Number.isFinite(pending.createdAt)
}

export function loadPendingChoice(scope: ChoiceOperationScope, now = Date.now()): PendingChoiceOperation | null {
  const target = storage()
  if (!target) return null
  const key = storageKey(scope)
  const raw = target.getItem(key)
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isPendingChoice(value) || scopeKey(value) !== scopeKey(scope) || value.createdAt > now ||
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

export function getOrCreatePendingChoice(scope: ChoiceOperationScope): PendingChoiceOperation {
  const prior = loadPendingChoice(scope)
  if (prior) return prior
  const pending: PendingChoiceOperation = {
    ...scope,
    key: `choice:${crypto.randomUUID()}`,
    createdAt: Date.now(),
  }
  storage()?.setItem(storageKey(scope), JSON.stringify(pending))
  return pending
}

export function clearPendingChoice(scope: ChoiceOperationScope, expectedKey: string): void {
  const target = storage()
  if (!target) return
  const pending = loadPendingChoice(scope)
  if (pending?.key === expectedKey) target.removeItem(storageKey(scope))
}

export function clearAllPendingChoices(): void {
  const target = storage()
  if (!target) return
  const keys: string[] = []
  for (let index = 0; index < target.length; index += 1) {
    const key = target.key(index)
    if (key?.startsWith(STORAGE_PREFIX)) keys.push(key)
  }
  for (const key of keys) target.removeItem(key)
}

export function isValidChoiceResponse(
  value: unknown,
  scope: ChoiceOperationScope,
  operationID: string,
): value is ChoiceOperationResponse {
  if (!value || typeof value !== 'object') return false
  const response = value as Partial<ChoiceOperationResponse>
  return response.request_id === operationID &&
    response.problem_id === scope.problemId &&
    response.session_id === scope.sessionId &&
    response.generation === scope.generation &&
    typeof response.success === 'boolean'
}

export function canApplyChoiceResult(
  current: Session | null,
  scope: ChoiceOperationScope,
  response: ChoiceOperationResponse,
): boolean {
  if (current === null) return false
  return current.problem_id === scope.problemId &&
    current.session_id === scope.sessionId && current.generation === scope.generation &&
    response.problem_id === scope.problemId && response.session_id === scope.sessionId &&
    response.generation === scope.generation
}

export class ChoiceOperationController {
  private inFlight: { scope: string; promise: Promise<ChoiceOperationResponse> } | null = null

  reset(): void {
    this.inFlight = null
  }

  async execute(
    scope: ChoiceOperationScope,
    request: (operationID: string) => Promise<unknown>,
  ): Promise<ChoiceOperationResponse> {
    const currentScope = scopeKey(scope)
    if (this.inFlight?.scope === currentScope) return this.inFlight.promise

    const pending = getOrCreatePendingChoice(scope)
    const promise = request(pending.key).then((value) => {
      if (!isValidChoiceResponse(value, scope, pending.key)) {
        throw new Error('서버가 올바른 답안 제출 응답을 반환하지 않았습니다. 같은 요청으로 다시 시도합니다.')
      }
      clearPendingChoice(scope, pending.key)
      return value
    }, (error: unknown) => {
      if (error instanceof APIResponseError) clearPendingChoice(scope, pending.key)
      throw error
    }).finally(() => {
      if (this.inFlight?.promise === promise) this.inFlight = null
    })
    this.inFlight = { scope: currentScope, promise }
    return promise
  }
}
