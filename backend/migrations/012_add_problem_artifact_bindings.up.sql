CREATE TABLE problem_artifacts (
    problem_id VARCHAR(255) NOT NULL,
    problem_revision VARCHAR(255) NOT NULL
        CHECK (problem_revision ~ '^[0-9a-f]{64}$'),
    artifact_digest_schema SMALLINT NOT NULL
        CHECK (artifact_digest_schema > 0),
    artifact_digest BYTEA NOT NULL
        CHECK (octet_length(artifact_digest) = 32),
    artifact_media_type TEXT NOT NULL
        CHECK (artifact_media_type <> '' AND octet_length(artifact_media_type) <= 255),
    artifact_size BIGINT NOT NULL
        CHECK (artifact_size > 0 AND artifact_size <= 8388608),
    source_trust TEXT NOT NULL
        CHECK (source_trust = 'development_checkout'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (problem_id, problem_revision),
    UNIQUE (artifact_digest_schema, artifact_digest),
    CONSTRAINT problem_artifacts_identity_fk
        FOREIGN KEY (problem_id)
        REFERENCES problems (id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Existing version-11 publications predate durable artifacts and cannot be
-- assigned bytes by migration code. NOT VALID preserves those rows while
-- enforcing the binding for every publication entry inserted from now on.
-- Startup may backfill only an exact current-head checkout after CAS read-back
-- verification; historical gaps remain explicit and fail closed when needed.
ALTER TABLE problem_catalog_entries
    ADD CONSTRAINT problem_catalog_entries_artifact_fk
    FOREIGN KEY (problem_id, problem_revision)
    REFERENCES problem_artifacts (problem_id, problem_revision)
    MATCH FULL ON UPDATE RESTRICT ON DELETE RESTRICT
    NOT VALID;

CREATE FUNCTION guard_problem_artifact_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('k8s-quiz.problem-catalog'));
    RETURN NEW;
END;
$$;

CREATE TRIGGER problem_artifacts_insert_guard
BEFORE INSERT ON problem_artifacts
FOR EACH ROW EXECUTE FUNCTION guard_problem_artifact_insert();

CREATE TRIGGER problem_artifacts_no_update_delete
BEFORE UPDATE OR DELETE ON problem_artifacts
FOR EACH ROW EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE TRIGGER problem_artifacts_no_truncate
BEFORE TRUNCATE ON problem_artifacts
FOR EACH STATEMENT EXECUTE FUNCTION reject_problem_catalog_mutation();

-- A version-11 head may intentionally remain unmapped until its exact source
-- bytes are recovered. Such a head can be inspected or backfilled, but must
-- never admit a fresh durable session.
CREATE OR REPLACE FUNCTION guard_session_catalog_selection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM pg_advisory_xact_lock_shared(hashtext('k8s-quiz.problem-catalog'));
        IF NEW.catalog_generation IS NULL THEN
            RAISE EXCEPTION 'new durable sessions require a catalog generation'
                USING ERRCODE = '23514';
        END IF;
        IF NOT EXISTS (
            SELECT 1
              FROM problem_catalog_head h
              JOIN problem_catalog_entries e
                ON e.catalog_generation = h.catalog_generation
              JOIN problem_artifacts a
                ON a.problem_id = e.problem_id
               AND a.problem_revision = e.problem_revision
             WHERE h.singleton = TRUE
               AND h.catalog_generation = NEW.catalog_generation
               AND e.problem_id = NEW.problem_id
               AND e.problem_revision = NEW.problem_revision
        ) THEN
            RAISE EXCEPTION 'new durable session catalog selection is not the current artifact-bound head'
                USING ERRCODE = '23503';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.catalog_generation IS DISTINCT FROM OLD.catalog_generation
       OR NEW.problem_id IS DISTINCT FROM OLD.problem_id
       OR NEW.problem_revision IS DISTINCT FROM OLD.problem_revision THEN
        RAISE EXCEPTION 'durable session catalog selection is immutable'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

REVOKE UPDATE, DELETE, TRUNCATE ON problem_artifacts FROM PUBLIC;
