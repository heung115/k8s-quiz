\set ON_ERROR_STOP on

-- Identifiers are supplied by psql --set. Credentials are deliberately absent:
-- operators provision LOGIN passwords through their secret manager or PostgreSQL
-- administration channel, never through this repository or this script.
SELECT CASE
         WHEN :'owner_role' ~ '^[a-z][a-z0-9_]{0,62}$'
          AND :'migrator_role' ~ '^[a-z][a-z0-9_]{0,62}$'
          AND :'runtime_role' ~ '^[a-z][a-z0-9_]{0,62}$'
          AND :'validator_role' ~ '^[a-z][a-z0-9_]{0,62}$'
          AND :'database_name' ~ '^[a-z][a-z0-9_]{0,62}$'
          AND cardinality(ARRAY[
                :'owner_role', :'migrator_role', :'runtime_role', :'validator_role'
              ]) = cardinality(ARRAY(
                SELECT DISTINCT role_name
                  FROM unnest(ARRAY[
                    :'owner_role', :'migrator_role', :'runtime_role', :'validator_role'
                  ]) AS role_name
              ))
         THEN 'SELECT true'
         ELSE 'SELECT pg_catalog.current_setting(''invalid Control Plane PostgreSQL identifier'')'
       END AS identifier_check_sql,
       pg_catalog.quote_ident(:'owner_role') AS owner_role_sql,
       pg_catalog.quote_ident(:'migrator_role') AS migrator_role_sql,
       pg_catalog.quote_ident(:'runtime_role') AS runtime_role_sql,
       pg_catalog.quote_ident(:'validator_role') AS validator_role_sql,
       pg_catalog.quote_ident(:'database_name') AS database_name_sql
\gset
:identifier_check_sql;

SELECT format(
    'CREATE ROLE %s NOLOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',
    :'owner_role_sql'
) WHERE NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=:'owner_role'
) \gexec

SELECT format(
    'CREATE ROLE %I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',
    role_name
)
FROM (VALUES (:'migrator_role'), (:'runtime_role'), (:'validator_role')) AS roles(role_name)
WHERE NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=role_name
) \gexec

ALTER ROLE :owner_role_sql NOLOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE :migrator_role_sql LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE :runtime_role_sql LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE :validator_role_sql LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;

REVOKE :owner_role_sql FROM :runtime_role_sql, :validator_role_sql;
GRANT :owner_role_sql TO :migrator_role_sql WITH INHERIT FALSE, SET TRUE;

-- pg_stat_activity hides another login's backend_start from ordinary roles.
-- The SECURITY DEFINER proof consumer must see the exact lease backend tuple,
-- so only its NOLOGIN owner receives the predefined read-stats membership.
GRANT pg_read_all_stats TO :owner_role_sql WITH INHERIT TRUE, SET FALSE;

-- Database CONNECT is cluster-scoped. This bootstrap is safe only on a
-- PostgreSQL cluster/container dedicated to the Control Plane, so close every
-- connectable non-template database before restoring the target allowlist.
SELECT format(
    'REVOKE ALL PRIVILEGES ON DATABASE %I FROM PUBLIC, %I, %I, %I',
    datname, :'migrator_role', :'runtime_role', :'validator_role'
)
FROM pg_catalog.pg_database
WHERE datallowconn AND NOT datistemplate
ORDER BY datname
\gexec

GRANT CONNECT ON DATABASE :database_name_sql TO :migrator_role_sql, :runtime_role_sql, :validator_role_sql;

ALTER SCHEMA public OWNER TO :owner_role_sql;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO :runtime_role_sql, :validator_role_sql;
