LOCK TABLE runner_controller_epochs,
           runner_controller_proofs,
           runner_controller_proof_consumptions
    IN ACCESS EXCLUSIVE MODE;

DO $migration$
BEGIN
    IF EXISTS (
        SELECT 1 FROM runner_controller_epochs
        WHERE lease_id IS NOT NULL
           OR lease_backend_pid IS NOT NULL
           OR lease_backend_start IS NOT NULL
           OR lease_advisory_key IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'cannot remove controller proof boundary while a controller lease is active'
            USING ERRCODE = '55000';
    END IF;
    IF EXISTS (SELECT 1 FROM runner_controller_proofs)
       OR EXISTS (SELECT 1 FROM runner_controller_proof_consumptions) THEN
        RAISE EXCEPTION 'cannot remove nonempty controller proof ledger'
            USING ERRCODE = '55000';
    END IF;
END
$migration$;

DROP FUNCTION consume_runner_controller_proof(
    TEXT, BIGINT, UUID, UUID, TEXT, BYTEA, TIMESTAMPTZ, TIMESTAMPTZ, TIMESTAMPTZ,
    TEXT, TEXT, BYTEA
);
DROP TABLE runner_controller_proof_consumptions;
DROP TABLE runner_controller_proofs;
ALTER TABLE runner_controller_epochs
    DROP CONSTRAINT runner_controller_epochs_lease_shape,
    DROP COLUMN lease_advisory_key,
    DROP COLUMN lease_backend_start,
    DROP COLUMN lease_backend_pid,
    DROP COLUMN lease_id;
