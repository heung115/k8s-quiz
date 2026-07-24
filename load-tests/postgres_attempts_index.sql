\set ON_ERROR_STOP on

-- Reproducible local query-plan exercise for the two real access patterns in
-- backend/internal/problem/repository.go:
--   1. latest in-progress attempt for one user
--   2. all attempts for one user ordered by start time
--
-- The transaction is rolled back, so synthetic rows and benchmark-only
-- indexes never remain in the development database.

BEGIN;
SET LOCAL client_min_messages = warning;

INSERT INTO users (id, github_id, username)
SELECT
  md5('loadtest-user-' || series)::uuid,
  900000000000000 + series,
  'loadtest-user-' || series
FROM generate_series(1, 100) AS series;

INSERT INTO problems (id, title)
SELECT
  'loadtest-problem-' || series,
  'Load-test problem ' || series
FROM generate_series(1, 10) AS series;

INSERT INTO attempts (
  id,
  user_id,
  problem_id,
  status,
  started_at,
  created_at
)
SELECT
  gen_random_uuid(),
  md5('loadtest-user-' || (((series - 1) % 100) + 1))::uuid,
  'loadtest-problem-' || (((series - 1) % 10) + 1),
  CASE (((series - 1) / 100) % 5)
    WHEN 0 THEN 'in_progress'
    WHEN 1 THEN 'success'
    WHEN 2 THEN 'failed'
    WHEN 3 THEN 'timeout'
    ELSE 'success'
  END,
  clock_timestamp() - (series * interval '1 millisecond'),
  clock_timestamp() - (series * interval '1 millisecond')
FROM generate_series(1, 100000) AS series;

ANALYZE attempts;

\echo '=== dataset ==='
SELECT
  COUNT(*) AS synthetic_attempts,
  COUNT(DISTINCT user_id) AS synthetic_users
FROM attempts
WHERE problem_id LIKE 'loadtest-problem-%';

\echo '=== baseline: latest active attempt ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, user_id, problem_id, status, started_at
FROM attempts
WHERE user_id = md5('loadtest-user-1')::uuid
  AND status = 'in_progress'
ORDER BY started_at DESC
LIMIT 1;

\echo '=== baseline: ordered attempt history ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, user_id, problem_id, status, started_at
FROM attempts
WHERE user_id = md5('loadtest-user-1')::uuid
ORDER BY started_at DESC;

CREATE INDEX bench_idx_attempts_active_user_started
  ON attempts (user_id, started_at DESC)
  WHERE status = 'in_progress';

CREATE INDEX bench_idx_attempts_user_started
  ON attempts (user_id, started_at DESC);

ANALYZE attempts;

\echo '=== optimized: latest active attempt ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, user_id, problem_id, status, started_at
FROM attempts
WHERE user_id = md5('loadtest-user-1')::uuid
  AND status = 'in_progress'
ORDER BY started_at DESC
LIMIT 1;

\echo '=== optimized: ordered attempt history ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, user_id, problem_id, status, started_at
FROM attempts
WHERE user_id = md5('loadtest-user-1')::uuid
ORDER BY started_at DESC;

ROLLBACK;
