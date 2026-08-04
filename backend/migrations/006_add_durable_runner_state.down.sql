DROP INDEX attempts_one_active_durable_per_user;
DROP INDEX attempts_one_per_durable_session;

ALTER TABLE attempts
    DROP CONSTRAINT attempts_session_allocation_fk,
    DROP CONSTRAINT attempts_session_generation_shape,
    DROP COLUMN generation,
    DROP COLUMN session_id;

DROP TABLE session_events;
DROP TABLE runner_operations;

ALTER TABLE sessions
    DROP CONSTRAINT sessions_current_allocation_fk;

DROP INDEX runner_allocations_recovery_work;
DROP INDEX runner_allocations_external_identity;
DROP INDEX runner_allocations_one_active_generation;
DROP TABLE runner_allocations;

DROP INDEX sessions_recovery_work;
DROP INDEX sessions_one_active_per_user;
DROP TABLE sessions;
