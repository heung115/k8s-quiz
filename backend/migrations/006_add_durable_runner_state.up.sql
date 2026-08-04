CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    problem_id VARCHAR(255) NOT NULL REFERENCES problems(id) ON DELETE RESTRICT,
    problem_revision VARCHAR(255) NOT NULL CHECK (problem_revision <> ''),
    current_generation BIGINT NOT NULL CHECK (current_generation > 0),
    state VARCHAR(32) NOT NULL CHECK (state IN (
        'queued', 'provisioning', 'booting', 'setting_up', 'ready',
        'verifying', 'completed', 'failed', 'timed_out',
        'provider_lost', 'destroying', 'destroyed'
    )),
    desired_state VARCHAR(16) NOT NULL CHECK (desired_state IN ('active', 'absent')),
    queued_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    lock_version BIGINT NOT NULL DEFAULT 0 CHECK (lock_version >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > queued_at),
    CHECK (finished_at IS NULL OR finished_at >= queued_at),
    UNIQUE (id, current_generation)
);

CREATE UNIQUE INDEX sessions_one_active_per_user
    ON sessions (user_id)
    WHERE desired_state = 'active';

CREATE INDEX sessions_recovery_work
    ON sessions (desired_state, expires_at, state);

CREATE TABLE runner_allocations (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    generation BIGINT NOT NULL CHECK (generation > 0),
    provider_kind VARCHAR(32) NOT NULL CHECK (provider_kind IN ('local-docker', 'home-proxmox', 'cloud')),
    provider_id VARCHAR(128) NOT NULL CHECK (provider_id <> ''),
    resource_profile VARCHAR(64) NOT NULL CHECK (resource_profile <> ''),
    external_id TEXT CHECK (external_id IS NULL OR external_id <> ''),
    desired_state VARCHAR(16) NOT NULL CHECK (desired_state IN ('active', 'absent')),
    observed_state VARCHAR(16) NOT NULL CHECK (observed_state IN (
        'unknown', 'provisioning', 'running', 'stopped',
        'deleting', 'absent', 'error'
    )),
    expires_at TIMESTAMPTZ NOT NULL,
    last_observed_at TIMESTAMPTZ,
    failure_code VARCHAR(64),
    failure_message VARCHAR(1024),
    last_event_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_event_sequence >= 0),
    lock_version BIGINT NOT NULL DEFAULT 0 CHECK (lock_version >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (session_id, generation),
    UNIQUE (id, provider_id)
);

CREATE UNIQUE INDEX runner_allocations_one_active_generation
    ON runner_allocations (session_id)
    WHERE desired_state = 'active';

CREATE UNIQUE INDEX runner_allocations_external_identity
    ON runner_allocations (provider_id, external_id)
    WHERE external_id IS NOT NULL;

CREATE INDEX runner_allocations_recovery_work
    ON runner_allocations (provider_id, desired_state, observed_state, expires_at);

ALTER TABLE sessions
    ADD CONSTRAINT sessions_current_allocation_fk
    FOREIGN KEY (id, current_generation)
    REFERENCES runner_allocations (session_id, generation)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE runner_operations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    allocation_id UUID NOT NULL,
    provider_id VARCHAR(128) NOT NULL,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('create', 'verify', 'destroy')),
    idempotency_key VARCHAR(255) NOT NULL CHECK (idempotency_key <> ''),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    state VARCHAR(24) NOT NULL CHECK (state IN (
        'pending', 'running', 'succeeded', 'failed',
        'cleanup_required', 'cancelled'
    )),
    error_code VARCHAR(64),
    error_message VARCHAR(1024),
    result_metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (pg_column_size(result_metadata) <= 16384),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ,
    lease_token UUID,
    lease_owner VARCHAR(128),
    lease_expires_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT runner_operations_allocation_provider_fk
        FOREIGN KEY (allocation_id, provider_id)
        REFERENCES runner_allocations (id, provider_id)
        ON DELETE RESTRICT,
    CONSTRAINT runner_operations_lease_shape CHECK (
        (lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL)
        OR
        (lease_token IS NOT NULL AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CONSTRAINT runner_operations_completion_shape CHECK (
        (state IN ('succeeded', 'failed', 'cancelled') AND completed_at IS NOT NULL)
        OR
        (state IN ('pending', 'running', 'cleanup_required') AND completed_at IS NULL)
    ),
    UNIQUE (provider_id, idempotency_key)
);

CREATE INDEX runner_operations_claimable
    ON runner_operations (state, next_attempt_at, lease_expires_at, created_at)
    WHERE state IN ('pending', 'running', 'cleanup_required');

CREATE TABLE session_events (
    allocation_id UUID NOT NULL REFERENCES runner_allocations(id) ON DELETE RESTRICT,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN (
        'allocation_reserved', 'vm_created', 'guest_connected', 'booting',
        'setup_running', 'ready', 'terminal_opened', 'terminal_closed',
        'verify_started', 'verify_finished', 'reset_requested',
        'timeout_warning', 'failed', 'timed_out', 'provider_lost',
        'destroying', 'destroyed', 'cleanup_required'
    )),
    reason_code VARCHAR(64),
    message VARCHAR(1024) NOT NULL DEFAULT '',
    sanitized_payload JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (pg_column_size(sanitized_payload) <= 16384),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (allocation_id, sequence)
);

ALTER TABLE attempts
    ADD COLUMN session_id UUID,
    ADD COLUMN generation BIGINT;

ALTER TABLE attempts
    ADD CONSTRAINT attempts_session_generation_shape CHECK (
        (session_id IS NULL AND generation IS NULL)
        OR
        (session_id IS NOT NULL AND generation IS NOT NULL AND generation > 0)
    ),
    ADD CONSTRAINT attempts_session_allocation_fk
        FOREIGN KEY (session_id, generation)
        REFERENCES runner_allocations (session_id, generation)
        ON DELETE RESTRICT;

CREATE UNIQUE INDEX attempts_one_per_durable_session
    ON attempts (session_id, generation)
    WHERE session_id IS NOT NULL;

CREATE UNIQUE INDEX attempts_one_active_durable_per_user
    ON attempts (user_id)
    WHERE status = 'in_progress' AND session_id IS NOT NULL;
