DO $migration$
BEGIN
    RAISE EXCEPTION 'controller proof post-lock expiry validation is irreversible; apply a forward fix or restore a tested backup'
        USING ERRCODE = '55000';
END
$migration$;
