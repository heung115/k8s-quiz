export interface User {
  id: string
  github_id: number
  username: string
  email: string
  avatar_url: string
  role: 'admin' | 'user'
  created_at: string
  updated_at: string
}

export interface Choice {
  id: string
  text: string
}

export interface Problem {
  id: string
  title: string
  description: string
  category: string
  difficulty: 'easy' | 'medium' | 'hard'
  type: 'fix' | 'find' | 'deploy'
  timeout_minutes: number
  verify_type: 'script' | 'choice' | 'text'
  base_image?: string
  image?: string
  choices?: Choice[]
  hint?: string
  created_at: string
}

export interface Attempt {
  id: string
  user_id: string
  problem_id: string
  status: 'in_progress' | 'success' | 'failed' | 'timeout'
  started_at: string
  finished_at?: string
  duration_seconds?: number
  verify_log?: string
}

export interface Session {
  request_id?: string
  operation_id: string
  session_id: string
  problem_id: string
  generation: number
  status: SessionStatus
  timeout_at: string | null
  cleanup_pending: boolean
  terminal_reason: string | null
  event_sequence: number
}

export interface Progress {
  solved_count: number
  total_attempts: number
  solved: Record<string, boolean>
}

export interface WSMessage {
	type: 'auth' | 'input' | 'resize' | 'output' | 'stage' | 'verify_result' | 'timeout_warning' | 'session_ended' | 'error'
	data?: string
	token?: string
	cols?: number
	rows?: number
	stage?: string
	message?: string
	success?: boolean
	log?: string
	reason?: string
	remaining_seconds?: number
	session_id?: string
	generation?: number
}

export interface TerminalOutputMessage {
  type: 'output'
  data: string
}

export interface TerminalErrorMessage {
  type: 'error'
  message: string
}

export interface TerminalAttachedMessage {
  type: 'terminal_attached'
  session_id: string
  generation: number
  attach_nonce: string
}

export type TerminalServerMessage = TerminalOutputMessage | TerminalErrorMessage | TerminalAttachedMessage

export interface TerminalReadyMessage {
  type: 'terminal_ready'
  attach_nonce: string
}

export interface TerminalInputMessage {
  type: 'input'
  attach_nonce: string
  data: string
}

export interface TerminalResizeMessage {
  type: 'resize'
  attach_nonce: string
  cols: number
  rows: number
}

export type TerminalClientMessage = TerminalReadyMessage | TerminalInputMessage | TerminalResizeMessage

export const WS_CLOSE_CONNECTION_REPLACED = 4001

export const SESSION_STATUSES = [
  'creating',
  'queued',
  'provisioning',
  'booting',
  'setting_up',
  'ready',
  'verifying',
  'completed',
  'failed',
  'timeout',
  'timed_out',
  'provider_lost',
  'destroying',
  'destroyed',
] as const

export type SessionStatus = (typeof SESSION_STATUSES)[number]

export const LIFECYCLE_SCHEMA = 'k8s-quiz.lifecycle/v1' as const

export const LIFECYCLE_STATUSES = [
  'queued',
  'provisioning',
  'booting',
  'setting_up',
  'ready',
  'verifying',
  'completed',
  'failed',
  'timed_out',
  'provider_lost',
  'destroying',
  'destroyed',
] as const

export type LifecycleStatus = (typeof LIFECYCLE_STATUSES)[number]

export interface LifecycleCursor {
  session_id: string
  generation: number
  event_sequence: number
}

export interface LifecycleVerifyResult {
  success: boolean
  log: string
}

export interface LifecycleSessionSnapshot {
  session_id: string
  problem_id: string
  generation: number
  operation_id: string
  status: LifecycleStatus
  timeout_at: string | null
  cleanup_pending: boolean
  terminal_reason: string | null
  latest_verify_result?: LifecycleVerifyResult | null
}

export interface LifecycleSnapshotFrame {
  type: 'lifecycle_snapshot'
  schema: typeof LIFECYCLE_SCHEMA
  session: LifecycleSessionSnapshot | null
  cursor: LifecycleCursor | null
}

export interface LifecycleEventFrame {
  type: 'lifecycle_event'
  schema: typeof LIFECYCLE_SCHEMA
  session_id: string
  generation: number
  event_sequence: number
  event_type: string
  reason_code: string
  message: string
  occurred_at: string
  payload: Record<string, unknown>
}

export interface LifecycleResyncRequiredFrame {
  type: 'lifecycle_resync_required'
  reason: string
}

export type LifecycleServerMessage =
  | LifecycleSnapshotFrame
  | LifecycleEventFrame
  | LifecycleResyncRequiredFrame

export interface LifecycleSubscribeFrame {
  type: 'lifecycle_subscribe'
  cursor: LifecycleCursor
}

export interface LeaderboardEntry {
	rank: number
	user_id: string
	username: string
	avatar_url: string
	solved_count: number
	total_attempts: number
}

export interface Achievement {
	id: string
	title: string
	description: string
	unlocked: boolean
}
