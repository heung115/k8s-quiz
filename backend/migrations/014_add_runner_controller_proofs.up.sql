ALTER TABLE runner_controller_epochs
    ADD COLUMN lease_id UUID,
    ADD COLUMN lease_backend_pid INTEGER,
    ADD COLUMN lease_backend_start TIMESTAMPTZ,
    ADD COLUMN lease_advisory_key BIGINT,
    ADD CONSTRAINT runner_controller_epochs_lease_shape CHECK (
        (lease_id IS NULL
         AND lease_backend_pid IS NULL
         AND lease_backend_start IS NULL
         AND lease_advisory_key IS NULL)
        OR
        (lease_id IS NOT NULL
         AND lease_backend_pid IS NOT NULL
         AND lease_backend_pid > 0
         AND lease_backend_start IS NOT NULL
         AND lease_advisory_key IS NOT NULL)
    );

CREATE TABLE runner_controller_proofs (
    provider_id VARCHAR(128) NOT NULL
        REFERENCES runner_controller_epochs(provider_id) ON DELETE RESTRICT,
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    lease_id UUID NOT NULL,
    proof_id UUID NOT NULL,
    operation VARCHAR(16) NOT NULL CHECK (operation IN ('activate','create','get','destroy')),
    request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
    effect_deadline TIMESTAMPTZ NOT NULL,
    issuer_uri VARCHAR(512) NOT NULL CHECK (issuer_uri <> ''),
    audience_uri VARCHAR(512) NOT NULL CHECK (audience_uri <> ''),
    token_hash BYTEA NOT NULL CHECK (octet_length(token_hash) = 32),
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider_id, proof_id),
    UNIQUE (provider_id, token_hash),
    CHECK (expires_at > issued_at),
    CHECK (expires_at <= effect_deadline),
    CHECK (expires_at - issued_at <= INTERVAL '5 seconds')
);

CREATE INDEX runner_controller_proofs_expiry
    ON runner_controller_proofs (expires_at);

CREATE TABLE runner_controller_proof_consumptions (
    provider_id VARCHAR(128) NOT NULL,
    proof_id UUID NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (provider_id, proof_id),
    FOREIGN KEY (provider_id, proof_id)
        REFERENCES runner_controller_proofs(provider_id, proof_id)
        ON DELETE RESTRICT
);

-- A validator connects with a dedicated least-privilege role and receives only
-- EXECUTE on this function. The function is generated with the migration's
-- exact target schema quoted into every relation reference; SECURITY DEFINER
-- execution therefore never resolves objects through a caller-writable schema.
-- The caller hashes the bearer token before invoking this boundary so pgcrypto's
-- installation schema is not part of the authority contract.
DO $migration$
DECLARE
    target_schema NAME := current_schema();
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'runner proof migration requires a current schema';
    END IF;

    EXECUTE format($ddl$
        CREATE FUNCTION %1$I.consume_runner_controller_proof(
            p_provider_id TEXT,
            p_epoch BIGINT,
            p_lease_id UUID,
            p_proof_id UUID,
            p_operation TEXT,
            p_request_digest BYTEA,
            p_effect_deadline TIMESTAMPTZ,
            p_issued_at TIMESTAMPTZ,
            p_expires_at TIMESTAMPTZ,
            p_issuer_uri TEXT,
            p_audience_uri TEXT,
            p_token_hash BYTEA
        ) RETURNS TEXT
        LANGUAGE plpgsql
        SECURITY DEFINER
        SET search_path TO pg_catalog
        AS $function$
        DECLARE
            authority_epoch BIGINT;
            authority_lease_id UUID;
            authority_backend_pid INTEGER;
            authority_backend_start TIMESTAMPTZ;
            authority_advisory_key BIGINT;
            proof_epoch BIGINT;
            proof_lease_id UUID;
            proof_operation TEXT;
            proof_request_digest BYTEA;
            proof_effect_deadline TIMESTAMPTZ;
            proof_expires_at TIMESTAMPTZ;
            proof_issuer_uri TEXT;
            proof_audience_uri TEXT;
            proof_token_hash BYTEA;
            proof_issued_at TIMESTAMPTZ;
            inserted_rows INTEGER;
            validation_timestamp TIMESTAMPTZ := clock_timestamp();
        BEGIN
            IF pg_is_in_recovery() THEN
                RETURN 'unavailable';
            END IF;

            SELECT epoch, lease_id, lease_backend_pid, lease_backend_start, lease_advisory_key
              INTO authority_epoch, authority_lease_id, authority_backend_pid,
                   authority_backend_start, authority_advisory_key
              FROM %1$I.runner_controller_epochs
             WHERE provider_id = p_provider_id
             FOR SHARE;

            IF NOT FOUND
               OR authority_epoch IS DISTINCT FROM p_epoch
               OR authority_lease_id IS DISTINCT FROM p_lease_id
               OR authority_backend_pid IS NULL
               OR authority_backend_start IS NULL
               OR authority_advisory_key IS NULL THEN
                RETURN 'fenced';
            END IF;

            SELECT epoch, lease_id, operation, request_digest, effect_deadline,
                   expires_at, issuer_uri, audience_uri, token_hash, issued_at
              INTO proof_epoch, proof_lease_id, proof_operation, proof_request_digest,
                   proof_effect_deadline, proof_expires_at, proof_issuer_uri,
                   proof_audience_uri, proof_token_hash, proof_issued_at
              FROM %1$I.runner_controller_proofs
             WHERE provider_id = p_provider_id AND proof_id = p_proof_id;

            IF NOT FOUND
               OR proof_epoch IS DISTINCT FROM p_epoch
               OR proof_lease_id IS DISTINCT FROM p_lease_id
               OR proof_operation IS DISTINCT FROM p_operation
               OR proof_request_digest IS DISTINCT FROM p_request_digest
               OR proof_effect_deadline IS DISTINCT FROM p_effect_deadline
               OR proof_issued_at IS DISTINCT FROM p_issued_at
               OR proof_expires_at IS DISTINCT FROM p_expires_at
               OR proof_issuer_uri IS DISTINCT FROM p_issuer_uri
               OR proof_audience_uri IS DISTINCT FROM p_audience_uri
               OR proof_token_hash IS DISTINCT FROM p_token_hash
               OR octet_length(p_token_hash) <> 32
               OR proof_issued_at > validation_timestamp
               OR proof_expires_at <= validation_timestamp
               OR proof_effect_deadline <= validation_timestamp THEN
                RETURN 'unauthorized';
            END IF;

            IF NOT EXISTS (
                SELECT 1
                  FROM pg_catalog.pg_stat_activity activity
                  JOIN pg_catalog.pg_locks held_lock ON held_lock.pid = activity.pid
                 WHERE activity.datname = current_database()
                   AND activity.pid = authority_backend_pid
                   AND activity.backend_start = authority_backend_start
                   AND held_lock.locktype = 'advisory'
                   AND held_lock.database = (
                       SELECT oid FROM pg_catalog.pg_database
                        WHERE datname = current_database()
                   )
                   AND held_lock.classid::BIGINT = ((authority_advisory_key >> 32) & 4294967295)
                   AND held_lock.objid::BIGINT = (authority_advisory_key & 4294967295)
                   AND held_lock.objsubid = 1
                   AND held_lock.mode = 'ExclusiveLock'
                   AND held_lock.granted
            ) THEN
                RETURN 'fenced';
            END IF;

            INSERT INTO %1$I.runner_controller_proof_consumptions
                (provider_id, proof_id, consumed_at)
            VALUES (p_provider_id, p_proof_id, validation_timestamp)
            ON CONFLICT DO NOTHING;
            GET DIAGNOSTICS inserted_rows = ROW_COUNT;
            IF inserted_rows <> 1 THEN
                RETURN 'replayed';
            END IF;
            RETURN 'accepted';
        END
        $function$
    $ddl$, target_schema);

    EXECUTE format(
        'REVOKE ALL ON FUNCTION %I.consume_runner_controller_proof(TEXT, BIGINT, UUID, UUID, TEXT, BYTEA, TIMESTAMPTZ, TIMESTAMPTZ, TIMESTAMPTZ, TEXT, TEXT, BYTEA) FROM PUBLIC',
        target_schema
    );
END
$migration$;
