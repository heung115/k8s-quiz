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
  session_id: string
  problem_id: string
  status: string
  timeout_at: string
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
