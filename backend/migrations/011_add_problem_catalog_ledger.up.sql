CREATE TABLE problem_catalog_publications (
    generation BIGINT PRIMARY KEY CHECK (generation > 0),
    previous_generation BIGINT,
    digest_schema SMALLINT NOT NULL CHECK (digest_schema > 0),
    candidate_digest BYTEA NOT NULL CHECK (octet_length(candidate_digest) = 32),
    entry_count INTEGER NOT NULL CHECK (entry_count > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT problem_catalog_publications_previous_fk
        FOREIGN KEY (previous_generation)
        REFERENCES problem_catalog_publications (generation)
        ON UPDATE RESTRICT ON DELETE RESTRICT,
    CONSTRAINT problem_catalog_publications_chain_shape CHECK (
        (generation = 1 AND previous_generation IS NULL)
        OR
        (generation > 1 AND previous_generation = generation - 1)
    ),
    UNIQUE (digest_schema, candidate_digest)
);

CREATE TABLE problem_catalog_entries (
    catalog_generation BIGINT NOT NULL,
    problem_id VARCHAR(255) NOT NULL,
    problem_revision VARCHAR(255) NOT NULL CHECK (problem_revision ~ '^[0-9a-f]{64}$'),
    title VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category VARCHAR(50) NOT NULL,
    difficulty VARCHAR(20) NOT NULL,
    type VARCHAR(20) NOT NULL,
    timeout_minutes INTEGER NOT NULL CHECK (timeout_minutes > 0),
    verify_type VARCHAR(20) NOT NULL,
    base_image VARCHAR(255) NOT NULL DEFAULT '',
    image VARCHAR(255) NOT NULL DEFAULT '',
    choices JSONB NOT NULL DEFAULT '[]'::jsonb,
    correct_choice VARCHAR(10) NOT NULL DEFAULT '',
    hint TEXT NOT NULL DEFAULT '',
    grading_prompt TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (catalog_generation, problem_id),
    CONSTRAINT problem_catalog_entries_publication_fk
        FOREIGN KEY (catalog_generation)
        REFERENCES problem_catalog_publications (generation)
        ON UPDATE RESTRICT ON DELETE RESTRICT,
    CONSTRAINT problem_catalog_entries_identity_fk
        FOREIGN KEY (problem_id)
        REFERENCES problems (id)
        ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE (catalog_generation, problem_id, problem_revision)
);

CREATE INDEX problem_catalog_entries_problem_revision_idx
    ON problem_catalog_entries (problem_id, problem_revision, catalog_generation);

CREATE TABLE problem_catalog_head (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    catalog_generation BIGINT NOT NULL UNIQUE,
    CONSTRAINT problem_catalog_head_publication_fk
        FOREIGN KEY (catalog_generation)
        REFERENCES problem_catalog_publications (generation)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

ALTER TABLE sessions
    ADD COLUMN catalog_generation BIGINT;

CREATE INDEX sessions_catalog_selection_idx
    ON sessions (catalog_generation, problem_id, problem_revision);

ALTER TABLE sessions
    ADD CONSTRAINT sessions_catalog_selection_fk
    FOREIGN KEY (catalog_generation, problem_id, problem_revision)
    REFERENCES problem_catalog_entries (catalog_generation, problem_id, problem_revision)
    MATCH SIMPLE ON UPDATE RESTRICT ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE sessions VALIDATE CONSTRAINT sessions_catalog_selection_fk;

CREATE FUNCTION reject_problem_catalog_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME
        USING ERRCODE = '55000';
END;
$$;

-- Every writer participates in the same database-wide fence as session
-- reservation. The application also holds a session-level exclusive lock
-- through activation; these transaction locks make direct/older writers obey
-- the same publication ordering.
CREATE FUNCTION guard_problem_catalog_publication_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    current_generation BIGINT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('k8s-quiz.problem-catalog'));

    SELECT catalog_generation INTO current_generation
      FROM problem_catalog_head
     WHERE singleton = TRUE;

    IF current_generation IS NULL THEN
        IF NEW.generation <> 1 OR NEW.previous_generation IS NOT NULL THEN
            RAISE EXCEPTION 'first problem catalog publication must be generation 1'
                USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.generation <> current_generation + 1
       OR NEW.previous_generation IS DISTINCT FROM current_generation THEN
        RAISE EXCEPTION 'problem catalog publication must extend the current head'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER problem_catalog_publications_insert_guard
BEFORE INSERT ON problem_catalog_publications
FOR EACH ROW EXECUTE FUNCTION guard_problem_catalog_publication_insert();

CREATE TRIGGER problem_catalog_publications_no_update_delete
BEFORE UPDATE OR DELETE ON problem_catalog_publications
FOR EACH ROW EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE TRIGGER problem_catalog_publications_no_truncate
BEFORE TRUNCATE ON problem_catalog_publications
FOR EACH STATEMENT EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE TRIGGER problem_catalog_entries_no_update_delete
BEFORE UPDATE OR DELETE ON problem_catalog_entries
FOR EACH ROW EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE TRIGGER problem_catalog_entries_no_truncate
BEFORE TRUNCATE ON problem_catalog_entries
FOR EACH STATEMENT EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE FUNCTION guard_problem_catalog_entry_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    current_generation BIGINT;
    publication_previous BIGINT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('k8s-quiz.problem-catalog'));

    SELECT catalog_generation INTO current_generation
      FROM problem_catalog_head
     WHERE singleton = TRUE;
    SELECT previous_generation INTO publication_previous
      FROM problem_catalog_publications
     WHERE generation = NEW.catalog_generation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'problem catalog publication % does not exist', NEW.catalog_generation
            USING ERRCODE = '23503';
    END IF;

    IF current_generation IS NULL THEN
        IF NEW.catalog_generation <> 1 OR publication_previous IS NOT NULL THEN
            RAISE EXCEPTION 'entry does not belong to the first pending publication'
                USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.catalog_generation <> current_generation + 1
       OR publication_previous IS DISTINCT FROM current_generation THEN
        RAISE EXCEPTION 'problem catalog generation % is already sealed or is not next',
            NEW.catalog_generation
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER problem_catalog_entries_insert_guard
BEFORE INSERT ON problem_catalog_entries
FOR EACH ROW EXECUTE FUNCTION guard_problem_catalog_entry_insert();

CREATE FUNCTION guard_problem_catalog_head()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    publication_previous BIGINT;
    publication_count INTEGER;
    actual_count INTEGER;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'problem_catalog_head cannot be deleted'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.singleton IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'problem_catalog_head singleton must be true'
            USING ERRCODE = '23514';
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.catalog_generation <> 1 THEN
            RAISE EXCEPTION 'first problem catalog generation must be 1'
                USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.catalog_generation <> OLD.catalog_generation + 1 THEN
        RAISE EXCEPTION 'problem catalog head must advance exactly one generation'
            USING ERRCODE = '23514';
    END IF;

    SELECT previous_generation, entry_count
      INTO publication_previous, publication_count
      FROM problem_catalog_publications
     WHERE generation = NEW.catalog_generation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'problem catalog publication % does not exist', NEW.catalog_generation
            USING ERRCODE = '23503';
    END IF;

    IF TG_OP = 'INSERT' AND publication_previous IS NOT NULL THEN
        RAISE EXCEPTION 'first problem catalog publication has a predecessor'
            USING ERRCODE = '23514';
    ELSIF TG_OP = 'UPDATE' AND publication_previous IS DISTINCT FROM OLD.catalog_generation THEN
        RAISE EXCEPTION 'problem catalog publication predecessor does not match head'
            USING ERRCODE = '23514';
    END IF;

    SELECT COUNT(*) INTO actual_count
      FROM problem_catalog_entries
     WHERE catalog_generation = NEW.catalog_generation;
    IF actual_count <> publication_count THEN
        RAISE EXCEPTION 'problem catalog publication % expected % entries but has %',
            NEW.catalog_generation, publication_count, actual_count
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER problem_catalog_head_guard
BEFORE INSERT OR UPDATE OR DELETE ON problem_catalog_head
FOR EACH ROW EXECUTE FUNCTION guard_problem_catalog_head();

CREATE TRIGGER problem_catalog_head_no_truncate
BEFORE TRUNCATE ON problem_catalog_head
FOR EACH STATEMENT EXECUTE FUNCTION reject_problem_catalog_mutation();

CREATE FUNCTION validate_problem_catalog_publication_complete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    actual_count INTEGER;
    final_head BIGINT;
BEGIN
    SELECT COUNT(*) INTO actual_count
      FROM problem_catalog_entries
     WHERE catalog_generation = NEW.generation;
    IF actual_count <> NEW.entry_count THEN
        RAISE EXCEPTION 'problem catalog publication % expected % entries but has %',
            NEW.generation, NEW.entry_count, actual_count
            USING ERRCODE = '23514';
    END IF;
    SELECT catalog_generation INTO final_head
      FROM problem_catalog_head
     WHERE singleton = TRUE;
    IF final_head IS DISTINCT FROM NEW.generation THEN
        RAISE EXCEPTION 'problem catalog publication % was not atomically installed as head',
            NEW.generation
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER problem_catalog_publication_complete
AFTER INSERT ON problem_catalog_publications
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_problem_catalog_publication_complete();

CREATE FUNCTION guard_session_catalog_selection()
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

CREATE TRIGGER sessions_catalog_selection_guard
BEFORE INSERT OR UPDATE ON sessions
FOR EACH ROW EXECUTE FUNCTION guard_session_catalog_selection();

REVOKE UPDATE, DELETE, TRUNCATE ON problem_catalog_publications FROM PUBLIC;
REVOKE UPDATE, DELETE, TRUNCATE ON problem_catalog_entries FROM PUBLIC;
REVOKE DELETE, TRUNCATE ON problem_catalog_head FROM PUBLIC;
