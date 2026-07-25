#!/bin/sh
# First-boot schema load for the postgres container: applies every
# /migrations/*.up.sql in lexicographic (migration order) sequence, then
# seeds golang-migrate's schema_migrations bookkeeping table so the backend's
# embedded migration runner (pkg/dbmigrate) sees the schema as current and
# does NOT replay the non-idempotent 001_init on first boot.
# Runs ONLY during the initdb phase, i.e. when the pgdata volume is empty.
# Schema upgrades on an existing volume are applied by the backend runner.
set -e

MAX=0
for f in /migrations/*.up.sql; do
  [ -e "$f" ] || continue
  echo "db-init: applying $(basename "$f")"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -f "$f"
  n=$(basename "$f" | sed -n 's/^\([0-9][0-9]*\)_.*\.up\.sql$/\1/p')
  [ -n "$n" ] && [ "$((10#$n))" -gt "$MAX" ] && MAX=$((10#$n))
done

# Mark the schema current for the embedded runner (version = max applied,
# dirty = false). ON CONFLICT keeps re-runs harmless.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
CREATE TABLE IF NOT EXISTS schema_migrations (
  version bigint NOT NULL PRIMARY KEY,
  dirty   boolean NOT NULL
);
INSERT INTO schema_migrations (version, dirty)
VALUES ($MAX, false)
ON CONFLICT (version) DO UPDATE SET dirty = false;
SQL

echo "db-init: schema up to date (schema_migrations at version $MAX)"
