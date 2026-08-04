DROP INDEX IF EXISTS session_events_allocation_event_key;

ALTER TABLE session_events
    DROP COLUMN IF EXISTS event_key;
