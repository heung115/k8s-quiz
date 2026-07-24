CREATE INDEX idx_attempts_active_user_started_at
ON attempts (user_id, started_at DESC)
WHERE status = 'in_progress';
