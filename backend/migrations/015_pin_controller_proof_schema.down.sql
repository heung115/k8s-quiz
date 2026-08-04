DO $migration$
BEGIN
    RAISE EXCEPTION 'controller proof schema pinning is irreversible; apply a forward fix or restore a tested backup'
        USING ERRCODE = '55000';
END
$migration$;
