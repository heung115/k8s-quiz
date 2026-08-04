-- Artifact bindings are the only durable route from a historical revision to
-- its exact runtime bytes. Never discard them through an automatic downgrade.
LOCK TABLE problem_artifacts IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM problem_artifacts) THEN
        RAISE EXCEPTION 'cannot remove problem artifact bindings after artifacts have been published';
    END IF;
END;
$$;

ALTER TABLE problem_catalog_entries
    DROP CONSTRAINT problem_catalog_entries_artifact_fk;

DROP TRIGGER problem_artifacts_no_truncate ON problem_artifacts;
DROP TRIGGER problem_artifacts_no_update_delete ON problem_artifacts;
DROP TRIGGER problem_artifacts_insert_guard ON problem_artifacts;
DROP FUNCTION guard_problem_artifact_insert();
DROP TABLE problem_artifacts;

-- Restore the version-11 guard after the artifact table no longer exists.
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
             WHERE h.singleton = TRUE
               AND h.catalog_generation = NEW.catalog_generation
               AND e.problem_id = NEW.problem_id
               AND e.problem_revision = NEW.problem_revision
        ) THEN
            RAISE EXCEPTION 'new durable session catalog selection is not the current head'
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
