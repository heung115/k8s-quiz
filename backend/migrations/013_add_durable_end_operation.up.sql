ALTER TABLE runner_operations
    DROP CONSTRAINT runner_operations_kind_check;

ALTER TABLE runner_operations
    ADD CONSTRAINT runner_operations_kind_check
    CHECK (kind IN ('create', 'verify', 'destroy', 'end'));

ALTER TABLE session_events
    DROP CONSTRAINT session_events_event_type_check;

ALTER TABLE session_events
    ADD CONSTRAINT session_events_event_type_check
    CHECK (event_type IN (
        'allocation_reserved', 'vm_created', 'guest_connected', 'booting',
        'setup_running', 'ready', 'terminal_opened', 'terminal_closed',
        'verify_started', 'verify_finished', 'reset_requested',
        'timeout_warning', 'failed', 'timed_out', 'provider_lost',
        'destroying', 'destroyed', 'cleanup_required', 'predecessor_destroyed'
    ));
