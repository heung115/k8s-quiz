ALTER TABLE problems
    ADD COLUMN IF NOT EXISTS catalog_active BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE problems
    ADD CONSTRAINT problems_active_revision_check
    CHECK (NOT catalog_active OR revision <> '');

CREATE INDEX IF NOT EXISTS problems_catalog_active_idx
    ON problems (catalog_active, created_at DESC);
