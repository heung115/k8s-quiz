DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runner_operations WHERE kind = 'end') THEN
        RAISE EXCEPTION 'cannot remove durable end operation support while end operations exist';
    END IF;
    IF EXISTS (SELECT 1 FROM session_events WHERE event_type = 'predecessor_destroyed') THEN
        RAISE EXCEPTION 'cannot remove predecessor cleanup events while they exist';
    END IF;
END $$;

ALTER TABLE runner_operations
    DROP CONSTRAINT runner_operations_kind_check;

ALTER TABLE runner_operations
    ADD CONSTRAINT runner_operations_kind_check
    CHECK (kind IN ('create', 'verify', 'destroy'));

ALTER TABLE session_events
    DROP CONSTRAINT session_events_event_type_check;

ALTER TABLE session_events
    ADD CONSTRAINT session_events_event_type_check
    CHECK (event_type IN (
        'allocation_reserved', 'vm_created', 'guest_connected', 'booting',
        'setup_running', 'ready', 'terminal_opened', 'terminal_closed',
        'verify_started', 'verify_finished', 'reset_requested',
        'timeout_warning', 'failed', 'timed_out', 'provider_lost',
        'destroying', 'destroyed', 'cleanup_required'
    ));
