-- Serialize the safety check with every session writer. Without this lock a
-- generation-bound reservation could commit after the check but before the
-- catalog_generation column is removed.
LOCK TABLE sessions IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM sessions WHERE catalog_generation IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot remove problem catalog ledger while generation-bound sessions exist';
    END IF;
END;
$$;

DROP TRIGGER sessions_catalog_selection_guard ON sessions;
DROP FUNCTION guard_session_catalog_selection();

ALTER TABLE sessions DROP CONSTRAINT sessions_catalog_selection_fk;
DROP INDEX sessions_catalog_selection_idx;
ALTER TABLE sessions DROP COLUMN catalog_generation;

DROP TRIGGER problem_catalog_publication_complete ON problem_catalog_publications;
DROP FUNCTION validate_problem_catalog_publication_complete();

DROP TRIGGER problem_catalog_head_no_truncate ON problem_catalog_head;
DROP TRIGGER problem_catalog_head_guard ON problem_catalog_head;
DROP FUNCTION guard_problem_catalog_head();

DROP TRIGGER problem_catalog_entries_no_truncate ON problem_catalog_entries;
DROP TRIGGER problem_catalog_entries_insert_guard ON problem_catalog_entries;
DROP TRIGGER problem_catalog_entries_no_update_delete ON problem_catalog_entries;
DROP TRIGGER problem_catalog_publications_no_truncate ON problem_catalog_publications;
DROP TRIGGER problem_catalog_publications_no_update_delete ON problem_catalog_publications;
DROP TRIGGER problem_catalog_publications_insert_guard ON problem_catalog_publications;

DROP TABLE problem_catalog_head;
DROP INDEX problem_catalog_entries_problem_revision_idx;
DROP TABLE problem_catalog_entries;
DROP TABLE problem_catalog_publications;

DROP FUNCTION guard_problem_catalog_entry_insert();
DROP FUNCTION guard_problem_catalog_publication_insert();
DROP FUNCTION reject_problem_catalog_mutation();
