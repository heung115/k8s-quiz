# Control Plane PostgreSQL bootstrap

`bootstrap-control-plane-roles.sql` creates and normalizes the PostgreSQL role
topology used by the Control Plane. Run it as the cluster provisioner while
connected to the target Control Plane database.

## Dedicated-cluster requirement

The database must run in a PostgreSQL cluster or container dedicated to this
Control Plane. Shared clusters are unsupported. Hardening changes
`pg_catalog` advisory-lock function ACLs, the `pg_read_all_stats` membership,
and `CONNECT` privileges for every connectable database in the cluster. Those
changes would affect unrelated applications on a shared cluster.

The bootstrap closes `PUBLIC`, migrator, runtime, and validator access to all
non-template databases, then grants those three login roles `CONNECT` only on
the named Control Plane database. The Go hardener repeats and verifies this
fail-closed boundary.

## Provisioning order

1. Create the dedicated database.
2. As the PostgreSQL provisioner, install `pgcrypto` in `public`:

   ```sql
   CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
   ```

3. Run the bootstrap with validated role and database identifiers.
4. Set login passwords through a secret manager or an administrative channel.
5. Run migrations with the migrator login. It changes to the `NOLOGIN` owner
   using `SET ROLE`; the owner deliberately has no database `CREATE` grant.
6. Run `control-plane-db harden --dedicated-postgres-cluster`, followed by
   `control-plane-db check` using the runtime and validator credentials.
7. Start the server with only the runtime URL and the five non-secret role
   names. Set `DATABASE_MIGRATION_MODE=external` and
   `DATABASE_DEDICATED_CLUSTER=true`. Never mount the migrator, provisioner, or
   validator URL into the server container.

`backend/Dockerfile` produces separate least-authority targets. The default
`server` target runs as UID/GID `10001` and contains only `/server`; `dbtool`
runs as the same non-root identity and contains only `/control-plane-db`.
`server-dev` is the explicit root-only Local Docker development target and
contains no database tool. Run `dbtool` only in separate one-shot jobs with
narrowly mounted mode-`0600` URL files owned by UID `10001`. Example flag shape
(role and file names are placeholders, not repository defaults).

`--catalog-owner-role` is not a fifth application role created by
`bootstrap-control-plane-roles.sql`. It must be the actual existing PostgreSQL
bootstrap/cluster role that owns every `pg_catalog` advisory-lock built-in on
this dedicated cluster (normally the provisioner login reported by
`SELECT current_user` on a fresh official PostgreSQL cluster). The hardener and
runtime attestation compare the configured name with those real owners. Do not
invent `kq_pg_catalog_owner`, grant it to another principal, or transfer
`pg_catalog` built-ins without a separately reviewed cluster-provisioning
procedure. In the examples below, `<actual-catalog-owner>` means that trusted
existing role.

```text
/control-plane-db migrate \
  --database-url-file=/run/secrets/control-plane-migrator-url \
  --owner-role=kq_cp_owner

/control-plane-db harden \
  --database-url-file=/run/secrets/control-plane-provisioner-url \
  --catalog-owner-role=<actual-catalog-owner> \
  --owner-role=kq_cp_owner \
  --migrator-role=kq_cp_migrator \
  --runtime-role=kq_cp_runtime \
  --validator-role=kq_cp_validator \
  --dedicated-postgres-cluster

/control-plane-db check \
  --runtime-database-url-file=/run/secrets/control-plane-runtime-url \
  --validator-database-url-file=/run/secrets/control-plane-validator-url \
  --catalog-owner-role=<actual-catalog-owner> \
  --owner-role=kq_cp_owner \
  --migrator-role=kq_cp_migrator \
  --runtime-role=kq_cp_runtime \
  --validator-role=kq_cp_validator
```

Server environment after the one-shot jobs:

```text
DATABASE_URL_FILE=/run/secrets/control-plane-runtime-url
DATABASE_MIGRATION_MODE=external
DATABASE_CATALOG_OWNER_ROLE=<actual-catalog-owner>
DATABASE_OWNER_ROLE=kq_cp_owner
DATABASE_MIGRATOR_ROLE=kq_cp_migrator
DATABASE_RUNTIME_ROLE=kq_cp_runtime
DATABASE_VALIDATOR_ROLE=kq_cp_validator
DATABASE_DEDICATED_CLUSTER=true
```

Public mode rejects a runtime URL supplied directly through `DATABASE_URL`.
The referenced file must be a regular, non-symlink, owner-only file containing
one value and the runtime URL must use `sslmode=verify-full`.

The server verifies migration version, role topology, object owners, exact
ACLs, advisory-function ownership and effective runtime denials before it
acquires the Runner controller lease.

Migration `001` retains `CREATE EXTENSION IF NOT EXISTS pgcrypto` for legacy
compatibility. It succeeds without owner `CREATE` authority only because the
provisioner has already installed the extension. A fresh deployment that skips
the provisioner step fails closed.

## Secrets

Do not pass passwords, database URLs, tokens, or other secrets to the bootstrap
script. It accepts identifiers only. Supply credentials using private
mode-`0600` files to the Go command or the deployment secret mechanism.
