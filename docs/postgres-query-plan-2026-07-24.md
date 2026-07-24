# PostgreSQL Attempt Query Plan — 2026-07-24

## Goal

`backend/internal/problem/repository.go`의 실제 조회 패턴을 합성 데이터로
재현하고, 측정 결과가 있는 경우에만 index를 추가한다.

대상 query는 사용자별 최신 `in_progress` attempt 1건을 찾는 로직이다.

```sql
SELECT id, user_id, problem_id, status, started_at
FROM attempts
WHERE user_id = $1
  AND status = 'in_progress'
ORDER BY started_at DESC
LIMIT 1;
```

## Reproduction

- PostgreSQL 16 Docker container
- 100 synthetic users
- 10 synthetic problems
- 100,000 synthetic attempts
- Script: `load-tests/postgres_attempts_index.sql`
- All synthetic rows and benchmark-only indexes were created inside a
  transaction and rolled back.

```bash
docker compose exec -T db \
  psql -U k8squiz -d k8squiz -f - \
  < load-tests/postgres_attempts_index.sql
```

## Baseline

The existing single-column `user_id` and `status` indexes produced a
`BitmapAnd`, read 200 matching rows, and then performed a top-N sort.

```text
Bitmap Heap Scan → Sort → Limit
rows read after filter: 200
execution time: 0.413 ms
shared buffers hit: 219
```

## Candidate Index

```sql
CREATE INDEX idx_attempts_active_user_started_at
ON attempts (user_id, started_at DESC)
WHERE status = 'in_progress';
```

The partial index stores only active attempts and already matches the requested
sort order.

```text
Index Scan → Limit
rows returned: 1
execution time: 0.032 ms
shared buffers: hit 1, read 2
```

In this local synthetic run, execution time decreased from 0.413 ms to
0.032 ms, about 12.9× faster. This is a query-plan exercise, not a production
performance claim.

## Rejected Index

A general `(user_id, started_at DESC)` index was also tested for the full
attempt-history query. PostgreSQL retained the existing bitmap scan and sort,
and the measured execution time did not improve. That index was not added.

## Applied Change

Only the measured partial index was added in migration
`003_add_active_attempt_lookup_index`.

## Boundaries

- Synthetic local data does not represent production traffic or storage.
- The exact timing depends on hardware, cache state, data distribution, and
  PostgreSQL configuration.
- The durable result is the ability to connect an application query to
  `EXPLAIN ANALYZE`, compare plans, reject an ineffective index, and keep only
  the measured change.
