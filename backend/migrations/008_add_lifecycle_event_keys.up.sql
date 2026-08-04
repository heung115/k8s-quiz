ALTER TABLE session_events
    ADD COLUMN event_key VARCHAR(255)
        CHECK (event_key IS NULL OR event_key <> '');

CREATE UNIQUE INDEX session_events_allocation_event_key
    ON session_events (allocation_id, event_key)
    WHERE event_key IS NOT NULL;
