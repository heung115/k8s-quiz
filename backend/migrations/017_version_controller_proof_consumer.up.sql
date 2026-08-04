DO $migration$
BEGIN
    IF current_schema() IS DISTINCT FROM 'public' THEN
        RAISE EXCEPTION 'controller proof v2 deployment migration requires fixed public schema'
            USING ERRCODE = '55000';
    END IF;
END
$migration$;

-- Version the security-sensitive consumer instead of trying to infer its
-- semantics from a mutable function body. V2 serializes on both authority and
-- proof rows, then samples time only after every potentially blocking lookup.
CREATE FUNCTION public.consume_runner_controller_proof_v2(
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
    proof_found BOOLEAN;
    inserted_rows INTEGER;
    validation_timestamp TIMESTAMPTZ;
BEGIN
    IF pg_is_in_recovery() THEN
        RETURN 'unavailable';
    END IF;

    SELECT epoch, lease_id, lease_backend_pid, lease_backend_start, lease_advisory_key
      INTO authority_epoch, authority_lease_id, authority_backend_pid,
           authority_backend_start, authority_advisory_key
      FROM public.runner_controller_epochs
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
      FROM public.runner_controller_proofs
     WHERE provider_id = p_provider_id AND proof_id = p_proof_id
     FOR UPDATE;
    proof_found := FOUND;

    IF NOT proof_found
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
       OR octet_length(p_token_hash) <> 32 THEN
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

    validation_timestamp := clock_timestamp();
    IF proof_issued_at > validation_timestamp
       OR proof_expires_at <= validation_timestamp
       OR proof_effect_deadline <= validation_timestamp THEN
        RETURN 'unauthorized';
    END IF;

    INSERT INTO public.runner_controller_proof_consumptions
        (provider_id, proof_id, consumed_at)
    VALUES (p_provider_id, p_proof_id, validation_timestamp)
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS inserted_rows = ROW_COUNT;
    IF inserted_rows <> 1 THEN
        RETURN 'replayed';
    END IF;
    RETURN 'accepted';
END
$function$;

REVOKE ALL ON FUNCTION public.consume_runner_controller_proof_v2(
    TEXT, BIGINT, UUID, UUID, TEXT, BYTEA, TIMESTAMPTZ, TIMESTAMPTZ,
    TIMESTAMPTZ, TEXT, TEXT, BYTEA
) FROM PUBLIC;

-- Remove the mutable legacy entry point so no role can continue using a
-- pre-v2 body after the migration is marked current.
DROP FUNCTION public.consume_runner_controller_proof(
    TEXT, BIGINT, UUID, UUID, TEXT, BYTEA, TIMESTAMPTZ, TIMESTAMPTZ,
    TIMESTAMPTZ, TEXT, TEXT, BYTEA
);
