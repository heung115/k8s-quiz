DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM runner_operations
        WHERE kind = 'verify' AND state = 'running'
        GROUP BY allocation_id
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot enforce running verify uniqueness: duplicate running verify operations require operator repair';
    END IF;
END
$$;

CREATE UNIQUE INDEX runner_operations_one_running_verify_per_allocation
    ON runner_operations (allocation_id)
    WHERE kind = 'verify' AND state = 'running';
