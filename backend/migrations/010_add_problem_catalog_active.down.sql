DROP INDEX IF EXISTS problems_catalog_active_idx;

ALTER TABLE problems
    DROP CONSTRAINT IF EXISTS problems_active_revision_check,
    DROP COLUMN IF EXISTS catalog_active;
