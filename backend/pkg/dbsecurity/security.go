// Package dbsecurity owns the PostgreSQL least-privilege boundary for the
// public-schema Control Plane database. It deliberately handles role names,
// never role passwords or raw connection strings.
package dbsecurity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	controlplanedb "github.com/heung115/k8s-quiz/runnerprotocol/controlplanedb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Schema                = "public"
	ProofConsumerIdentity = controlplanedb.ProofConsumerIdentity
)

var (
	ErrInvalidConfig = errors.New("invalid Control Plane PostgreSQL security configuration")
	identifierRE     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// Config names pre-created database roles. The database must be dedicated to
// the Control Plane because hardening removes database-wide PUBLIC grants and
// restricts pg_catalog advisory-lock functions.
type Config struct {
	CatalogOwnerRole string
	OwnerRole        string
	MigratorRole     string
	RuntimeRole      string
	ValidatorRole    string
	// DedicatedCluster confirms database-wide and pg_catalog ACL changes cannot
	// affect another application. Shared PostgreSQL clusters are unsupported.
	DedicatedCluster bool
}

var applicationTables = controlplanedb.ApplicationTables()
var applicationFunctions = controlplanedb.ApplicationFunctions()
var triggerFunctions = controlplanedb.TriggerFunctions()
var runtimeAdvisoryFunctions = controlplanedb.RuntimeAdvisoryFunctions()

// Migrations create UUID defaults while public precedes pg_catalog in the
// migrator search path. PostgreSQL therefore stores the pgcrypto extension
// function OID in those defaults. Runtime needs this one exact extension
// capability for INSERT defaults; no other pgcrypto function is executable.
var runtimeExtensionFunctions = controlplanedb.RuntimeExtensionFunctions()
var runtimePolicies = controlplanedb.RuntimePolicies()

func validateConfig(config Config) error {
	if !config.DedicatedCluster {
		return ErrInvalidConfig
	}
	roles := []string{config.CatalogOwnerRole, config.OwnerRole, config.MigratorRole, config.RuntimeRole, config.ValidatorRole}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if !identifierRE.MatchString(role) || seen[role] {
			return ErrInvalidConfig
		}
		seen[role] = true
	}
	return nil
}

func quoteIdentifier(value string) (string, error) {
	if !identifierRE.MatchString(value) {
		return "", ErrInvalidConfig
	}
	return pgx.Identifier{value}.Sanitize(), nil
}

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ConfigureRuntimePool fixes name resolution and verifies every new runtime
// connection before it can enter the pool.
func ConfigureRuntimePool(poolConfig *pgxpool.Config, config Config) error {
	return configurePool(poolConfig, config, config.RuntimeRole)
}

// ConfigureValidatorPool fixes name resolution and verifies every new proof
// validator connection before it can enter the pool.
func ConfigureValidatorPool(poolConfig *pgxpool.Config, config Config) error {
	return configurePool(poolConfig, config, config.ValidatorRole)
}

func configurePool(poolConfig *pgxpool.Config, config Config, expectedRole string) error {
	if poolConfig == nil || validateConfig(config) != nil {
		return ErrInvalidConfig
	}
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = "pg_catalog, public, pg_temp"
	priorAfterConnect := poolConfig.AfterConnect
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if priorAfterConnect != nil {
			if err := priorAfterConnect(ctx, conn); err != nil {
				return err
			}
		}
		return verifyConnectionIdentity(ctx, conn, expectedRole)
	}
	priorBeforeAcquire := poolConfig.BeforeAcquire
	poolConfig.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
		if priorBeforeAcquire != nil && !priorBeforeAcquire(ctx, conn) {
			return false
		}
		return verifyConnectionIdentity(ctx, conn, expectedRole) == nil
	}
	return nil
}

func verifyConnectionIdentity(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, expected string) error {
	var current, session, path string
	var login, inherit, super, createDB, createRole, replication, bypassRLS bool
	if err := q.QueryRow(ctx, `
		SELECT current_user,session_user,current_setting('search_path'),
		       role.rolcanlogin,role.rolinherit,role.rolsuper,role.rolcreatedb,
		       role.rolcreaterole,role.rolreplication,role.rolbypassrls
		FROM pg_catalog.pg_roles role WHERE role.rolname=current_user`).Scan(
		&current, &session, &path, &login, &inherit, &super, &createDB, &createRole, &replication, &bypassRLS,
	); err != nil {
		return fmt.Errorf("inspect PostgreSQL connection identity: %w", err)
	}
	if current != expected || session != expected || path != "pg_catalog, public, pg_temp" ||
		!login || inherit || super || createDB || createRole || replication || bypassRLS {
		return errors.New("PostgreSQL connection identity is not least privileged")
	}
	return nil
}

// Harden normalizes ownership, direct ACLs, default ACLs, and advisory-lock
// execution in one transaction. Role creation and credentials remain an
// operator/bootstrap responsibility.
func Harden(ctx context.Context, pool *pgxpool.Pool, config Config) error {
	if pool == nil || validateConfig(config) != nil {
		return ErrInvalidConfig
	}
	owner, _ := quoteIdentifier(config.OwnerRole)
	migrator, _ := quoteIdentifier(config.MigratorRole)
	runtime, _ := quoteIdentifier(config.RuntimeRole)
	validator, _ := quoteIdentifier(config.ValidatorRole)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Control Plane PostgreSQL hardening: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := verifyRoleTopology(ctx, tx, config); err != nil {
		return err
	}
	var database string
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return fmt.Errorf("read Control Plane database name: %w", err)
	}
	databaseID := pgx.Identifier{database}.Sanitize()

	// PUBLIC CONNECT is cluster-wide in effect because every database normally
	// inherits it. A dedicated cluster is required and every non-template DB is
	// closed before the exact Control Plane target grants are restored.
	databaseRows, err := tx.Query(ctx, `SELECT pg_catalog.format(
		'REVOKE ALL PRIVILEGES ON DATABASE %I FROM PUBLIC, %I, %I, %I',
		datname,$1::text,$2::text,$3::text)
		FROM pg_catalog.pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`,
		config.MigratorRole, config.RuntimeRole, config.ValidatorRole)
	if err != nil {
		return fmt.Errorf("inspect dedicated PostgreSQL cluster databases: %w", err)
	}
	var closeDatabaseStatements []string
	for databaseRows.Next() {
		var statement string
		if err := databaseRows.Scan(&statement); err != nil {
			databaseRows.Close()
			return fmt.Errorf("scan dedicated PostgreSQL cluster database: %w", err)
		}
		closeDatabaseStatements = append(closeDatabaseStatements, statement)
	}
	if err := databaseRows.Err(); err != nil {
		databaseRows.Close()
		return fmt.Errorf("iterate dedicated PostgreSQL cluster databases: %w", err)
	}
	databaseRows.Close()

	statements := append(closeDatabaseStatements, "ALTER SCHEMA public OWNER TO "+owner)
	for _, table := range applicationTables {
		statements = append(statements, "ALTER TABLE public."+pgx.Identifier{table}.Sanitize()+" OWNER TO "+owner)
	}
	for _, identity := range applicationFunctions {
		statements = append(statements, "ALTER FUNCTION "+identity+" OWNER TO "+owner)
	}
	for _, identity := range triggerFunctions {
		statements = append(statements, "ALTER FUNCTION "+identity+" SET search_path TO pg_catalog, public")
	}
	statements = append(statements,
		"ALTER FUNCTION "+ProofConsumerIdentity+" SET search_path TO pg_catalog",
		"REVOKE ALL PRIVILEGES ON DATABASE "+databaseID+" FROM PUBLIC",
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"ALTER ROLE "+runtime+" IN DATABASE "+databaseID+" SET search_path TO pg_catalog, public, pg_temp",
		"ALTER ROLE "+validator+" IN DATABASE "+databaseID+" SET search_path TO pg_catalog, public, pg_temp",
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" IN SCHEMA public REVOKE ALL ON TABLES FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC",
	)
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply Control Plane PostgreSQL hardening: %w", err)
		}
	}
	if err := normalizeACLs(ctx, tx, config); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "GRANT CONNECT ON DATABASE "+databaseID+" TO "+migrator+", "+runtime+", "+validator); err != nil {
		return fmt.Errorf("grant exact Control Plane database CONNECT authority: %w", err)
	}
	if _, err := tx.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+runtime+", "+validator); err != nil {
		return fmt.Errorf("grant exact Control Plane schema authority: %w", err)
	}
	if err := grantRuntimePolicy(ctx, tx, config); err != nil {
		return err
	}
	for identity := range runtimeExtensionFunctions {
		if _, err := tx.Exec(ctx, "GRANT EXECUTE ON FUNCTION "+identity+" TO "+runtime); err != nil {
			return fmt.Errorf("grant runtime extension function %s: %w", identity, err)
		}
	}
	if _, err := tx.Exec(ctx, "GRANT EXECUTE ON FUNCTION "+ProofConsumerIdentity+" TO "+validator); err != nil {
		return fmt.Errorf("grant proof consumption authority: %w", err)
	}
	for identity := range runtimeAdvisoryFunctions {
		if _, err := tx.Exec(ctx, "GRANT EXECUTE ON FUNCTION pg_catalog."+identity+" TO "+owner+", "+runtime); err != nil {
			return fmt.Errorf("grant runtime advisory function %s: %w", identity, err)
		}
	}
	if err := verifyCatalogSecurity(ctx, tx, config); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Control Plane PostgreSQL hardening: %w", err)
	}
	return nil
}

func normalizeACLs(ctx context.Context, tx pgx.Tx, config Config) error {
	// The PostgreSQL cluster is dedicated, so every non-owner grant on application
	// objects and every advisory-lock function is removed before exact grants
	// are rebuilt. Extension functions remain extension-owned but lose PUBLIC
	// execution in this database.
	queries := []struct {
		sql  string
		args []any
	}{
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES ON DATABASE %I FROM %s',d.datname,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_database d CROSS JOIN LATERAL pg_catalog.aclexplode(d.datacl) a
		  LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE d.datname=current_database() AND a.grantee<>d.datdba`},
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES ON SCHEMA public FROM %s',
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_namespace n CROSS JOIN LATERAL pg_catalog.aclexplode(n.nspacl) a
		  LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE n.nspname='public' AND a.grantee<>n.nspowner`},
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES ON TABLE public.%I FROM %s',c.relname,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		  CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE n.nspname='public' AND c.relname=ANY($1::text[]) AND a.grantee<>c.relowner`, args: []any{applicationTables}},
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES (%I) ON TABLE public.%I FROM %s',att.attname,c.relname,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_attribute att JOIN pg_catalog.pg_class c ON c.oid=att.attrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace CROSS JOIN LATERAL pg_catalog.aclexplode(att.attacl) a
		  LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE n.nspname='public' AND c.relname=ANY($1::text[]) AND att.attnum>0 AND NOT att.attisdropped
		   AND a.grantee<>c.relowner`, args: []any{applicationTables}},
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES ON FUNCTION %s FROM %s',p.oid::pg_catalog.regprocedure,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		  CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
		  LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE n.nspname='public' AND a.grantee<>p.proowner`},
		{sql: `SELECT pg_catalog.format('REVOKE ALL PRIVILEGES ON FUNCTION %s FROM %s',p.oid::pg_catalog.regprocedure,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',r.rolname) END)
		  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		  CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
		  LEFT JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		 WHERE n.nspname='pg_catalog' AND (p.proname LIKE 'pg_advisory_%' OR p.proname LIKE 'pg_try_advisory_%')
		   AND a.grantee<>p.proowner`},
		{sql: `SELECT pg_catalog.format('ALTER DEFAULT PRIVILEGES FOR ROLE %I%s REVOKE ALL ON %s FROM %s',owner.rolname,
			CASE WHEN d.defaclnamespace=0 THEN '' ELSE ' IN SCHEMA public' END,
			CASE d.defaclobjtype WHEN 'r' THEN 'TABLES' WHEN 'S' THEN 'SEQUENCES' WHEN 'f' THEN 'FUNCTIONS' END,
			CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE pg_catalog.format('%I',grantee.rolname) END)
		  FROM pg_catalog.pg_default_acl d JOIN pg_catalog.pg_roles owner ON owner.oid=d.defaclrole
		  LEFT JOIN pg_catalog.pg_namespace n ON n.oid=d.defaclnamespace
		  CROSS JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) a LEFT JOIN pg_catalog.pg_roles grantee ON grantee.oid=a.grantee
		 WHERE owner.rolname=$1 AND (d.defaclnamespace=0 OR n.nspname='public')
		   AND d.defaclobjtype IN ('r','S','f') AND a.grantee<>d.defaclrole`, args: []any{config.OwnerRole}},
	}
	for _, query := range queries {
		rows, err := tx.Query(ctx, query.sql, query.args...)
		if err != nil {
			return fmt.Errorf("inspect Control Plane ACLs for normalization: %w", err)
		}
		var statements []string
		for rows.Next() {
			var statement string
			if err := rows.Scan(&statement); err != nil {
				rows.Close()
				return fmt.Errorf("scan Control Plane ACL normalization: %w", err)
			}
			statements = append(statements, statement)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate Control Plane ACL normalization: %w", err)
		}
		rows.Close()
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("normalize Control Plane ACL: %w", err)
			}
		}
	}
	return nil
}

func grantRuntimePolicy(ctx context.Context, tx pgx.Tx, config Config) error {
	runtime, _ := quoteIdentifier(config.RuntimeRole)
	for _, table := range applicationTables {
		policy := runtimePolicies[table]
		qualified := "public." + pgx.Identifier{table}.Sanitize()
		var statements []string
		if policy.SelectAll {
			statements = append(statements, "GRANT SELECT ON "+qualified+" TO "+runtime)
		} else if len(policy.SelectColumns) != 0 {
			statements = append(statements, "GRANT SELECT ("+quotedColumns(policy.SelectColumns)+") ON "+qualified+" TO "+runtime)
		}
		if len(policy.InsertColumns) != 0 {
			statements = append(statements, "GRANT INSERT ("+quotedColumns(policy.InsertColumns)+") ON "+qualified+" TO "+runtime)
		}
		if len(policy.UpdateColumns) != 0 {
			statements = append(statements, "GRANT UPDATE ("+quotedColumns(policy.UpdateColumns)+") ON "+qualified+" TO "+runtime)
		}
		if policy.DeleteRows {
			statements = append(statements, "GRANT DELETE ON "+qualified+" TO "+runtime)
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("grant runtime policy on %s: %w", table, err)
			}
		}
	}
	return nil
}

func quotedColumns(columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = pgx.Identifier{column}.Sanitize()
	}
	return strings.Join(quoted, ",")
}

// Verify checks catalog state through a runtime connection and independently
// checks both login identities. It rejects missing and excess direct grants.
func Verify(ctx context.Context, runtime, validator *pgxpool.Pool, config Config) error {
	if runtime == nil || validator == nil || validateConfig(config) != nil {
		return ErrInvalidConfig
	}
	if err := VerifyRuntime(ctx, runtime, config); err != nil {
		return err
	}
	if err := verifyConnectionIdentity(ctx, validator, config.ValidatorRole); err != nil {
		return fmt.Errorf("verify validator connection: %w", err)
	}
	return verifyEffectiveDenials(ctx, runtime, validator, config)
}

// VerifyRuntime is the server startup attestation. It requires no validator
// credential: exact catalog ACLs prove the validator's only direct authority,
// while runtime effective denials are checked through the connected runtime
// role. The one-shot Verify additionally connects as validator.
func VerifyRuntime(ctx context.Context, runtime *pgxpool.Pool, config Config) error {
	if runtime == nil || validateConfig(config) != nil {
		return ErrInvalidConfig
	}
	if err := verifyConnectionIdentity(ctx, runtime, config.RuntimeRole); err != nil {
		return fmt.Errorf("verify runtime connection: %w", err)
	}
	if err := verifyCatalogSecurity(ctx, runtime, config); err != nil {
		return err
	}
	return verifyRuntimeEffectiveDenials(ctx, runtime)
}

func verifyRoleTopology(ctx context.Context, q queryer, config Config) error {
	rows, err := q.Query(ctx, `
		SELECT rolname,rolcanlogin,rolinherit,rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls
		FROM pg_catalog.pg_roles WHERE rolname=ANY($1::text[]) ORDER BY rolname`,
		[]string{config.OwnerRole, config.MigratorRole, config.RuntimeRole, config.ValidatorRole})
	if err != nil {
		return fmt.Errorf("inspect Control Plane roles: %w", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var role string
		var login, inherit, super, createDB, createRole, replication, bypassRLS bool
		if err := rows.Scan(&role, &login, &inherit, &super, &createDB, &createRole, &replication, &bypassRLS); err != nil {
			return fmt.Errorf("scan Control Plane role: %w", err)
		}
		wantLogin := role != config.OwnerRole
		wantInherit := role == config.OwnerRole
		if login != wantLogin || inherit != wantInherit || super || createDB || createRole || replication || bypassRLS {
			return fmt.Errorf("Control Plane role %s violates the role boundary", role)
		}
		seen++
	}
	if rows.Err() != nil || seen != 4 {
		return errors.New("Control Plane role inventory is incomplete")
	}

	type membership struct {
		granted, member     string
		admin, inherit, set bool
	}
	membershipRows, err := q.Query(ctx, `
		SELECT granted.rolname,member.rolname,m.admin_option,m.inherit_option,m.set_option
		FROM pg_catalog.pg_auth_members m
		JOIN pg_catalog.pg_roles granted ON granted.oid=m.roleid
		JOIN pg_catalog.pg_roles member ON member.oid=m.member
		WHERE granted.rolname=ANY($1::text[]) OR member.rolname=ANY($1::text[])
		ORDER BY 1,2`, []string{config.OwnerRole, config.MigratorRole, config.RuntimeRole, config.ValidatorRole})
	if err != nil {
		return fmt.Errorf("inspect Control Plane role memberships: %w", err)
	}
	defer membershipRows.Close()
	var got []membership
	for membershipRows.Next() {
		var item membership
		if err := membershipRows.Scan(&item.granted, &item.member, &item.admin, &item.inherit, &item.set); err != nil {
			return fmt.Errorf("scan Control Plane role membership: %w", err)
		}
		got = append(got, item)
	}
	want := []membership{
		{granted: config.OwnerRole, member: config.MigratorRole, inherit: false, set: true},
		{granted: "pg_read_all_stats", member: config.OwnerRole, inherit: true, set: false},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].granted+want[i].member < want[j].granted+want[j].member })
	if fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Errorf("Control Plane role membership graph is not exact: got %v", got)
	}
	return nil
}

func verifyCatalogSecurity(ctx context.Context, q queryer, config Config) error {
	if err := verifyRoleTopology(ctx, q, config); err != nil {
		return err
	}
	if err := verifyDedicatedCluster(ctx, q, config); err != nil {
		return err
	}
	if err := verifyDatabaseAndSchemaACL(ctx, q, config); err != nil {
		return err
	}
	if err := verifyRelations(ctx, q, config); err != nil {
		return err
	}
	if err := verifyFunctions(ctx, q, config); err != nil {
		return err
	}
	if err := verifyAdvisoryFunctions(ctx, q, config); err != nil {
		return err
	}
	return verifyDefaultACL(ctx, q, config)
}

func verifyDedicatedCluster(ctx context.Context, q queryer, config Config) error {
	var current string
	if err := q.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		return fmt.Errorf("read target database identity: %w", err)
	}
	rows, err := q.Query(ctx, `SELECT datname,
		pg_catalog.has_database_privilege('public',datname,'CONNECT'),
		pg_catalog.has_database_privilege($1,datname,'CONNECT'),
		pg_catalog.has_database_privilege($2,datname,'CONNECT'),
		pg_catalog.has_database_privilege($3,datname,'CONNECT')
		FROM pg_catalog.pg_database
		WHERE datallowconn AND NOT datistemplate
		ORDER BY datname`, config.MigratorRole, config.RuntimeRole, config.ValidatorRole)
	if err != nil {
		return fmt.Errorf("inspect dedicated PostgreSQL cluster topology: %w", err)
	}
	defer rows.Close()
	currentSeen := false
	for rows.Next() {
		var database string
		var publicConnect, migratorConnect, runtimeConnect, validatorConnect bool
		if err := rows.Scan(&database, &publicConnect, &migratorConnect, &runtimeConnect, &validatorConnect); err != nil {
			return fmt.Errorf("scan dedicated PostgreSQL cluster topology: %w", err)
		}
		if database == current {
			currentSeen = true
			if publicConnect || !migratorConnect || !runtimeConnect || !validatorConnect {
				return errors.New("target database CONNECT topology is unsafe")
			}
			continue
		}
		if publicConnect || migratorConnect || runtimeConnect || validatorConnect {
			return fmt.Errorf("Control Plane roles can connect to non-target database %s", database)
		}
	}
	if !currentSeen {
		return errors.New("target database is absent from cluster topology")
	}
	return nil
}

func verifyDatabaseAndSchemaACL(ctx context.Context, q queryer, config Config) error {
	var schemaOwner string
	if err := q.QueryRow(ctx, `SELECT owner.rolname FROM pg_catalog.pg_namespace n
		JOIN pg_catalog.pg_roles owner ON owner.oid=n.nspowner WHERE n.nspname='public'`).Scan(&schemaOwner); err != nil {
		return fmt.Errorf("inspect public schema owner: %w", err)
	}
	if schemaOwner != config.OwnerRole {
		return errors.New("public schema owner is unsafe")
	}
	allowedDB := map[string]bool{config.MigratorRole: true, config.RuntimeRole: true, config.ValidatorRole: true}
	rows, err := q.Query(ctx, `SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE role.rolname END,
		a.privilege_type,a.is_grantable,owner.rolname
		FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles owner ON owner.oid=d.datdba
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(d.datacl,pg_catalog.acldefault('d',d.datdba))) a
		LEFT JOIN pg_catalog.pg_roles role ON role.oid=a.grantee WHERE d.datname=current_database()`)
	if err != nil {
		return fmt.Errorf("inspect database ACL: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var grantee, privilege, owner string
		var grantable bool
		if err := rows.Scan(&grantee, &privilege, &grantable, &owner); err != nil {
			return err
		}
		if grantee == owner {
			continue
		}
		if !allowedDB[grantee] || privilege != "CONNECT" || grantable || seen[grantee] {
			return fmt.Errorf("unsafe database ACL for %s %s", grantee, privilege)
		}
		seen[grantee] = true
	}
	if len(seen) != len(allowedDB) {
		return errors.New("database CONNECT ACL is incomplete")
	}

	allowedSchema := map[string]bool{config.RuntimeRole: true, config.ValidatorRole: true}
	rows, err = q.Query(ctx, `SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE role.rolname END,
		a.privilege_type,a.is_grantable
		FROM pg_catalog.pg_namespace n CROSS JOIN LATERAL pg_catalog.aclexplode(
		COALESCE(n.nspacl,pg_catalog.acldefault('n',n.nspowner))) a
		LEFT JOIN pg_catalog.pg_roles role ON role.oid=a.grantee WHERE n.nspname='public'`)
	if err != nil {
		return fmt.Errorf("inspect public schema ACL: %w", err)
	}
	defer rows.Close()
	seen = map[string]bool{}
	for rows.Next() {
		var grantee, privilege string
		var grantable bool
		if err := rows.Scan(&grantee, &privilege, &grantable); err != nil {
			return err
		}
		if grantee == config.OwnerRole {
			continue
		}
		if !allowedSchema[grantee] || privilege != "USAGE" || grantable || seen[grantee] {
			return fmt.Errorf("unsafe public schema ACL for %s %s", grantee, privilege)
		}
		seen[grantee] = true
	}
	if len(seen) != len(allowedSchema) {
		return errors.New("public schema USAGE ACL is incomplete")
	}
	return nil
}

func verifyRelations(ctx context.Context, q queryer, config Config) error {
	rows, err := q.Query(ctx, `SELECT c.relname,owner.rolname,c.relkind::text
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=c.relowner
		WHERE n.nspname='public' AND c.relkind IN ('r','p') ORDER BY c.relname`)
	if err != nil {
		return fmt.Errorf("inspect application relation inventory: %w", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, owner, kind string
		if err := rows.Scan(&name, &owner, &kind); err != nil {
			return err
		}
		if owner != config.OwnerRole || (kind != "r" && kind != "p") {
			return fmt.Errorf("unsafe application relation %s", name)
		}
		got = append(got, name)
	}
	want := append([]string(nil), applicationTables...)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("application relation inventory mismatch: got %v", got)
	}

	for _, table := range applicationTables {
		if err := verifyTableACL(ctx, q, config, table, runtimePolicies[table]); err != nil {
			return err
		}
	}
	return nil
}

func verifyTableACL(ctx context.Context, q queryer, config Config, table string, policy controlplanedb.TablePolicy) error {
	rows, err := q.Query(ctx, `SELECT CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE role.rolname END,a.privilege_type,a.is_grantable
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))) a
		LEFT JOIN pg_catalog.pg_roles role ON role.oid=a.grantee
		WHERE n.nspname='public' AND c.relname=$1 ORDER BY 1,2`, table)
	if err != nil {
		return fmt.Errorf("inspect relation ACL %s: %w", table, err)
	}
	defer rows.Close()
	want := map[string]bool{}
	if policy.SelectAll {
		want["SELECT"] = true
	}
	if policy.DeleteRows {
		want["DELETE"] = true
	}
	seen := map[string]bool{}
	for rows.Next() {
		var grantee, privilege string
		var grantable bool
		if err := rows.Scan(&grantee, &privilege, &grantable); err != nil {
			return err
		}
		if grantee == config.OwnerRole {
			continue
		}
		if grantee != config.RuntimeRole || !want[privilege] || grantable || seen[privilege] {
			return fmt.Errorf("unsafe relation ACL on %s for %s %s", table, grantee, privilege)
		}
		seen[privilege] = true
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		return fmt.Errorf("relation ACL on %s is incomplete: got %v want %v", table, seen, want)
	}

	columnWant := map[string]map[string]bool{}
	for _, column := range policy.SelectColumns {
		columnWant[column] = map[string]bool{"SELECT": true}
	}
	for _, column := range policy.InsertColumns {
		if columnWant[column] == nil {
			columnWant[column] = map[string]bool{}
		}
		columnWant[column]["INSERT"] = true
	}
	for _, column := range policy.UpdateColumns {
		if columnWant[column] == nil {
			columnWant[column] = map[string]bool{}
		}
		columnWant[column]["UPDATE"] = true
	}
	rows, err = q.Query(ctx, `SELECT att.attname,CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE role.rolname END,
		a.privilege_type,a.is_grantable
		FROM pg_catalog.pg_attribute att JOIN pg_catalog.pg_class c ON c.oid=att.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace CROSS JOIN LATERAL pg_catalog.aclexplode(att.attacl) a
		LEFT JOIN pg_catalog.pg_roles role ON role.oid=a.grantee
		WHERE n.nspname='public' AND c.relname=$1 AND att.attnum>0 AND NOT att.attisdropped
		ORDER BY 1,2,3`, table)
	if err != nil {
		return fmt.Errorf("inspect column ACL %s: %w", table, err)
	}
	defer rows.Close()
	columnSeen := map[string]map[string]bool{}
	for rows.Next() {
		var column, grantee, privilege string
		var grantable bool
		if err := rows.Scan(&column, &grantee, &privilege, &grantable); err != nil {
			return err
		}
		if grantee == config.OwnerRole {
			continue
		}
		if grantee != config.RuntimeRole || !columnWant[column][privilege] || grantable {
			return fmt.Errorf("unsafe column ACL on %s.%s for %s %s", table, column, grantee, privilege)
		}
		if columnSeen[column] == nil {
			columnSeen[column] = map[string]bool{}
		}
		if columnSeen[column][privilege] {
			return fmt.Errorf("duplicate column ACL on %s.%s", table, column)
		}
		columnSeen[column][privilege] = true
	}
	if fmt.Sprint(columnSeen) != fmt.Sprint(columnWant) {
		return fmt.Errorf("column ACL on %s is incomplete: got %v want %v", table, columnSeen, columnWant)
	}
	return nil
}

func verifyFunctions(ctx context.Context, q queryer, config Config) error {
	// Require the exact non-extension function inventory. pgcrypto extension
	// members are allowed but are covered by the all-functions ACL audit below.
	rows, err := q.Query(ctx, `SELECT p.oid::pg_catalog.regprocedure::text,owner.rolname
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
		WHERE n.nspname='public' AND NOT EXISTS (
		 SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e')
		ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("inspect application function inventory: %w", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var identity, owner string
		if err := rows.Scan(&identity, &owner); err != nil {
			return err
		}
		if owner != config.OwnerRole {
			return fmt.Errorf("unsafe function owner on %s", identity)
		}
		got = append(got, identity)
	}
	want := append([]string(nil), applicationFunctions...)
	for i := range want {
		want[i] = strings.TrimPrefix(want[i], "public.")
	}
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("application function inventory mismatch: got %v want %v", got, want)
	}
	for _, identity := range triggerFunctions {
		var owner, result, language string
		var securityDefiner, fixedPath bool
		if err := q.QueryRow(ctx, `SELECT owner.rolname,pg_catalog.pg_get_function_result(p.oid),lang.lanname,
			p.prosecdef,COALESCE(p.proconfig=ARRAY['search_path=pg_catalog, public']::text[],FALSE)
			FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
			JOIN pg_catalog.pg_language lang ON lang.oid=p.prolang WHERE p.oid=pg_catalog.to_regprocedure($1)`, identity).
			Scan(&owner, &result, &language, &securityDefiner, &fixedPath); err != nil {
			return fmt.Errorf("inspect trigger function %s: %w", identity, err)
		}
		if owner != config.OwnerRole || result != "trigger" || language != "plpgsql" || securityDefiner || !fixedPath {
			return fmt.Errorf("unsafe trigger function %s", identity)
		}
	}
	if err := verifyProofConsumer(ctx, q, config); err != nil {
		return err
	}

	rows, err = q.Query(ctx, `SELECT p.oid::pg_catalog.regprocedure::text,owner.rolname,
		CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE grantee.rolname END,a.privilege_type,a.is_grantable,
		COALESCE(extension.extname,'')
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
		LEFT JOIN pg_catalog.pg_roles grantee ON grantee.oid=a.grantee
		LEFT JOIN pg_catalog.pg_depend dependency
		  ON dependency.classid='pg_catalog.pg_proc'::pg_catalog.regclass
		 AND dependency.objid=p.oid AND dependency.deptype='e'
		LEFT JOIN pg_catalog.pg_extension extension ON extension.oid=dependency.refobjid
		WHERE n.nspname='public' ORDER BY 1,3,4`)
	if err != nil {
		return fmt.Errorf("inspect public function ACLs: %w", err)
	}
	defer rows.Close()
	validatorExecute := 0
	runtimeExtensionExecute := map[string]bool{}
	for rows.Next() {
		var identity, owner, grantee, privilege, extension string
		var grantable bool
		if err := rows.Scan(&identity, &owner, &grantee, &privilege, &grantable, &extension); err != nil {
			return err
		}
		if grantee == owner {
			continue
		}
		if identity == strings.TrimPrefix(ProofConsumerIdentity, "public.") && grantee == config.ValidatorRole &&
			privilege == "EXECUTE" && !grantable {
			validatorExecute++
			continue
		}
		if runtimeExtensionFunctions[identity] && extension == "pgcrypto" && grantee == config.RuntimeRole &&
			privilege == "EXECUTE" && !grantable && !runtimeExtensionExecute[identity] {
			runtimeExtensionExecute[identity] = true
			continue
		}
		return fmt.Errorf("unsafe public function ACL on %s for %s", identity, grantee)
	}
	if validatorExecute != 1 {
		return errors.New("proof validator EXECUTE ACL is incomplete")
	}
	if len(runtimeExtensionExecute) != len(runtimeExtensionFunctions) {
		return errors.New("runtime extension EXECUTE ACL is incomplete")
	}
	return nil
}

func verifyProofConsumer(ctx context.Context, q queryer, config Config) error {
	var owner, language, result, body string
	var definer, fixedPath bool
	if err := q.QueryRow(ctx, `SELECT owner.rolname,lang.lanname,pg_catalog.pg_get_function_result(p.oid),
		p.prosrc,p.prosecdef,COALESCE(p.proconfig=ARRAY['search_path=pg_catalog']::text[],FALSE)
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
		JOIN pg_catalog.pg_language lang ON lang.oid=p.prolang WHERE p.oid=pg_catalog.to_regprocedure($1)`,
		ProofConsumerIdentity).Scan(&owner, &language, &result, &body, &definer, &fixedPath); err != nil {
		return fmt.Errorf("inspect controller proof consumer: %w", err)
	}
	if owner != config.OwnerRole || language != "plpgsql" || result != "text" || !definer || !fixedPath {
		return errors.New("controller proof consumer security attributes are unsafe")
	}
	if !controlplanedb.ProofConsumerBodyMatches(body) {
		return errors.New("controller proof consumer body does not match the v2 security contract")
	}
	for _, relation := range []string{"runner_controller_epochs", "runner_controller_proofs", "runner_controller_proof_consumptions"} {
		fixed := "public." + relation
		if !strings.Contains(body, fixed) {
			return fmt.Errorf("controller proof consumer lacks fixed reference %s", fixed)
		}
		scrubbed := strings.ReplaceAll(body, fixed, "")
		if regexp.MustCompile(`(?i)(^|[^a-z0-9_.])` + regexp.QuoteMeta(relation) + `([^a-z0-9_]|$)`).MatchString(scrubbed) {
			return fmt.Errorf("controller proof consumer has an unqualified reference to %s", relation)
		}
	}
	if regexp.MustCompile(`(?i)\bexecute\b`).MatchString(body) {
		return errors.New("controller proof consumer contains dynamic SQL")
	}
	return nil
}

func verifyAdvisoryFunctions(ctx context.Context, q queryer, config Config) error {
	rows, err := q.Query(ctx, `SELECT p.oid::pg_catalog.regprocedure::text,owner.rolname,
		CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE grantee.rolname END,a.privilege_type,a.is_grantable
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) a
		LEFT JOIN pg_catalog.pg_roles grantee ON grantee.oid=a.grantee
		WHERE n.nspname='pg_catalog' AND (p.proname LIKE 'pg_advisory_%' OR p.proname LIKE 'pg_try_advisory_%')
		ORDER BY 1,3`)
	if err != nil {
		return fmt.Errorf("inspect advisory function ACLs: %w", err)
	}
	defer rows.Close()
	seenOwner := map[string]bool{}
	seenRuntime := map[string]bool{}
	count := 0
	for rows.Next() {
		var identity, owner, grantee, privilege string
		var grantable bool
		if err := rows.Scan(&identity, &owner, &grantee, &privilege, &grantable); err != nil {
			return err
		}
		count++
		if owner != config.CatalogOwnerRole {
			return fmt.Errorf("unexpected advisory function owner on %s", identity)
		}
		if grantee == owner {
			continue
		}
		if !runtimeAdvisoryFunctions[identity] || privilege != "EXECUTE" || grantable {
			return fmt.Errorf("unsafe advisory function ACL on %s for %s", identity, grantee)
		}
		switch grantee {
		case config.OwnerRole:
			if seenOwner[identity] {
				return fmt.Errorf("duplicate owner advisory function ACL on %s", identity)
			}
			seenOwner[identity] = true
		case config.RuntimeRole:
			if seenRuntime[identity] {
				return fmt.Errorf("duplicate runtime advisory function ACL on %s", identity)
			}
			seenRuntime[identity] = true
		default:
			return fmt.Errorf("unsafe advisory function ACL on %s for %s", identity, grantee)
		}
	}
	if count == 0 || len(seenOwner) != len(runtimeAdvisoryFunctions) || len(seenRuntime) != len(runtimeAdvisoryFunctions) {
		return fmt.Errorf("advisory function ACL is incomplete: owner=%v runtime=%v", seenOwner, seenRuntime)
	}
	return nil
}

func verifyDefaultACL(ctx context.Context, q queryer, config Config) error {
	rows, err := q.Query(ctx, `SELECT d.defaclobjtype::text,n.nspname,
		CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE grantee.rolname END,a.privilege_type,a.is_grantable
		FROM pg_catalog.pg_default_acl d LEFT JOIN pg_catalog.pg_namespace n ON n.oid=d.defaclnamespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=d.defaclrole
		CROSS JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) a
		LEFT JOIN pg_catalog.pg_roles grantee ON grantee.oid=a.grantee
		WHERE owner.rolname=$1 AND (d.defaclnamespace=0 OR n.nspname='public') AND d.defaclobjtype IN ('r','S','f')`,
		config.OwnerRole)
	if err != nil {
		return fmt.Errorf("inspect default ACLs: %w", err)
	}
	defer rows.Close()
	functionOwnerExecute := 0
	for rows.Next() {
		var objectType, grantee, privilege string
		var namespace *string
		var grantable bool
		if err := rows.Scan(&objectType, &namespace, &grantee, &privilege, &grantable); err != nil {
			return err
		}
		if grantee != config.OwnerRole || grantable {
			return fmt.Errorf("unsafe default ACL for %s to %s", objectType, grantee)
		}
		if objectType == "f" && namespace == nil && privilege == "EXECUTE" {
			functionOwnerExecute++
		}
	}
	if functionOwnerExecute != 1 {
		return errors.New("function default ACL does not revoke PUBLIC exactly")
	}
	return nil
}

func verifyEffectiveDenials(ctx context.Context, runtime, validator *pgxpool.Pool, config Config) error {
	if err := verifyRuntimeEffectiveDenials(ctx, runtime); err != nil {
		return err
	}
	for _, role := range []string{config.RuntimeRole, config.ValidatorRole} {
		pool := runtime
		if role == config.ValidatorRole {
			pool = validator
		}
		var canCreateDB, canTemp, canCreateSchema bool
		if err := pool.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege(current_user,current_database(),'CREATE'),
			pg_catalog.has_database_privilege(current_user,current_database(),'TEMPORARY'),
			pg_catalog.has_schema_privilege(current_user,'public','CREATE')`).Scan(
			&canCreateDB, &canTemp, &canCreateSchema); err != nil {
			return err
		}
		if canCreateDB || canTemp || canCreateSchema {
			return fmt.Errorf("role %s has object-creation authority", role)
		}
	}
	for _, table := range applicationTables {
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "TRIGGER"} {
			var allowed bool
			if err := validator.QueryRow(ctx, `SELECT pg_catalog.has_table_privilege(current_user,$1,$2)`,
				"public."+table, privilege).Scan(&allowed); err != nil {
				return err
			}
			if allowed {
				return fmt.Errorf("validator can %s application table %s", privilege, table)
			}
		}
	}
	return nil
}

func verifyRuntimeEffectiveDenials(ctx context.Context, runtime *pgxpool.Pool) error {
	var canCreateDB, canTemp, canCreateSchema bool
	if err := runtime.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege(current_user,current_database(),'CREATE'),
		pg_catalog.has_database_privilege(current_user,current_database(),'TEMPORARY'),
		pg_catalog.has_schema_privilege(current_user,'public','CREATE')`).Scan(
		&canCreateDB, &canTemp, &canCreateSchema); err != nil {
		return err
	}
	if canCreateDB || canTemp || canCreateSchema {
		return errors.New("runtime has object-creation authority")
	}
	for _, table := range []string{"runner_controller_proofs", "runner_controller_proof_consumptions"} {
		for _, privilege := range []string{"UPDATE", "DELETE", "TRUNCATE", "TRIGGER"} {
			var allowed bool
			if err := runtime.QueryRow(ctx, `SELECT pg_catalog.has_table_privilege(current_user,$1,$2)`,
				"public."+table, privilege).Scan(&allowed); err != nil {
				return err
			}
			if allowed {
				return fmt.Errorf("runtime can %s proof ledger %s", privilege, table)
			}
		}
	}
	return nil
}
