-- AUTH-5: refresh token families for rotation + reuse detection.
-- A login creates a family; each rotation marks the old token used and issues
-- a new token in the same family. Presenting a used token revokes the family.
ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS family_id UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN IF NOT EXISTS used BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens(family_id);
