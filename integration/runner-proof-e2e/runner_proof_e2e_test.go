package runnerproofe2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	privateapi "github.com/heung115/k8s-quiz-infra/runner/privateapi/v1"
	"github.com/heung115/k8s-quiz-infra/runner/proxmox"
	controlplanedb "github.com/heung115/k8s-quiz/runnerprotocol/controlplanedb"
	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/dbsecurity"
)

const (
	databaseURLEnv          = "RUNNER_PROOF_E2E_DATABASE_URL"
	runtimeDatabaseURLEnv   = "RUNNER_PROOF_E2E_RUNTIME_DATABASE_URL"
	validatorDatabaseURLEnv = "RUNNER_PROOF_E2E_VALIDATOR_DATABASE_URL"

	clientURI  = "spiffe://k8s-quiz.test/control-plane"
	serverURI  = "spiffe://k8s-quiz.test/private-runner"
	serverName = "runner.internal.test"

	proofFunctionIdentity = controlplanedb.ProofConsumerIdentity
)

func TestRunnerAuthorityProofCrossRepositoryE2E(t *testing.T) {
	databaseURL := os.Getenv(databaseURLEnv)
	if databaseURL == "" {
		t.Skip(databaseURLEnv + " is not set")
	}
	runtimeURL, runtimeSet := os.LookupEnv(runtimeDatabaseURLEnv)
	validatorURL, validatorSet := os.LookupEnv(validatorDatabaseURLEnv)
	if runtimeSet != validatorSet {
		t.Skip(runtimeDatabaseURLEnv + " and " + validatorDatabaseURLEnv + " must be set together")
	}
	strictRoles := runtimeSet && validatorSet
	if !strictRoles {
		runtimeURL = databaseURL
		validatorURL = databaseURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adminPool := openPool(t, ctx, databaseURL, "pg_catalog,public")
	runtimePool := openPool(t, ctx, runtimeURL, "pg_catalog, public, pg_temp")
	validatorPool := openPool(t, ctx, validatorURL, "pg_catalog, public, pg_temp")
	strictSecurity := attestDatabaseLayout(t, ctx, adminPool, runtimePool, validatorPool, strictRoles)

	providerID := "home-proxmox:e2e-" + randomHex(t, 12)
	backend := &recordingBackend{}
	verifier, err := privateapi.NewPostgresLeaseProofVerifier(validatorPool, "public")
	if err != nil {
		t.Fatalf("construct proof verifier: %v", err)
	}
	handler, err := privateapi.NewServer(privateapi.ServerConfig{
		ExpectedClientURI: clientURI, ProviderID: providerID,
		AuthorityIssuerURI: clientURI, AuthorityAudienceURI: serverURI,
		ProofVerifier: verifier, MaxOperationTimeout: time.Minute,
	}, backend)
	if err != nil {
		t.Fatalf("construct private Runner server: %v", err)
	}

	pki := newEphemeralPKI(t)
	serverTLS, err := privateapi.NewServerTLSConfig(privateapi.ServerTLSOptions{
		Certificate: pki.serverCertificate, ServerCACertificates: []*x509.Certificate{pki.rootCertificate},
		ClientCACertificates: []*x509.Certificate{pki.rootCertificate},
		ExpectedClientURI:    clientURI, ExpectedServerURI: serverURI, ServerName: serverName,
	})
	if err != nil {
		t.Fatalf("construct server TLS config: %v", err)
	}
	serverTLS.MaxVersion = tls.VersionTLS13
	var nonTLS13 atomic.Bool
	tlsHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || request.TLS.Version != tls.VersionTLS13 {
			nonTLS13.Store(true)
			http.Error(writer, "TLS 1.3 required", http.StatusUpgradeRequired)
			return
		}
		handler.ServeHTTP(writer, request)
	})
	testServer := httptest.NewUnstartedServer(tlsHandler)
	testServer.TLS = serverTLS
	testServer.StartTLS()
	t.Cleanup(testServer.Close)

	clientTLS, err := protocol.NewClientTLSConfig(protocol.ClientTLSOptions{
		Certificate: pki.clientCertificate, RootCAs: pki.roots,
		ServerName: serverName, ExpectedServerURI: serverURI,
	})
	if err != nil {
		t.Fatalf("construct client TLS config: %v", err)
	}
	clientTLS.MaxVersion = tls.VersionTLS13
	rawTransport := &http.Transport{
		TLSClientConfig: clientTLS.Clone(), TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	rawClient := &http.Client{Transport: rawTransport, Timeout: 10 * time.Second}
	t.Cleanup(rawClient.CloseIdleConnections)

	oldLease, err := runner.AcquireControllerLease(ctx, runtimePool, providerID, func(loss error) {
		t.Errorf("old controller lease lost unexpectedly: %v", loss)
	})
	if err != nil {
		t.Fatalf("acquire old controller lease: %v", err)
	}
	oldLeaseClosed := false
	t.Cleanup(func() {
		if oldLeaseClosed {
			return
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := oldLease.Close(closeCtx); err != nil {
			t.Errorf("close old controller lease: %v", err)
		}
	})
	oldEpoch := oldLease.Fence().Epoch
	oldIssuer, err := runner.NewProtocolProofIssuer(oldLease)
	if err != nil {
		t.Fatalf("construct old protocol proof issuer: %v", err)
	}
	oldClient := newProtocolClient(t, testServer.URL, clientTLS, providerID, oldIssuer)

	// The supported path crosses both repositories and reaches the lifecycle
	// backend once with a server-created VerifiedAuthority.
	normalRequest := newCreateRequest(t, oldEpoch, "normal")
	normalBefore := backend.createCount()
	view, err := oldClient.Create(ctx, normalRequest)
	if err != nil {
		t.Fatalf("normal create: %v", err)
	}
	if view.AllocationID != normalRequest.AllocationID || view.VMPhase != "vm_running" ||
		view.LifecycleScope != protocol.LifecycleScopeVMOnly {
		t.Fatalf("normal create returned an invalid view: %+v", view)
	}
	backend.assertCreateDelta(t, normalBefore, 1, oldEpoch)

	// Replaying the exact proof and exact wire request is rejected online before
	// a second backend call, even though the first request was accepted.
	replayRequest := newCreateRequest(t, oldEpoch, "exact-replay")
	replayRequest.AuthorityProof = issueProof(t, ctx, oldIssuer, providerID, protocol.OperationCreate, replayRequest)
	replayBefore := backend.createCount()
	response := rawPost(t, ctx, rawClient, testServer.URL+protocol.PathCreate, replayRequest)
	assertSuccess(t, response)
	backend.assertCreateDelta(t, replayBefore, 1, oldEpoch)
	response = rawPost(t, ctx, rawClient, testServer.URL+protocol.PathCreate, replayRequest)
	assertWireError(t, response, http.StatusUnauthorized, protocol.CodeUnauthorized)
	backend.assertCreateDelta(t, replayBefore, 1, oldEpoch)

	// A proof is bound to the complete semantic request. A valid field mutation
	// retains a well-formed envelope but fails proof verification before backend.
	mutationRequest := newCreateRequest(t, oldEpoch, "mutation")
	mutationRequest.AuthorityProof = issueProof(t, ctx, oldIssuer, providerID, protocol.OperationCreate, mutationRequest)
	mutationRequest.ProblemID = "service-dns"
	mutationBefore := backend.createCount()
	response = rawPost(t, ctx, rawClient, testServer.URL+protocol.PathCreate, mutationRequest)
	assertWireError(t, response, http.StatusUnauthorized, protocol.CodeUnauthorized)
	backend.assertCreateDelta(t, mutationBefore, 0, 0)

	// A proof issued while N owns the lease cannot be admitted after that exact
	// lease is closed. The database reports fencing before backend execution.
	fencedRequest := newCreateRequest(t, oldEpoch, "closed-lease")
	fencedRequest.AuthorityProof = issueProof(t, ctx, oldIssuer, providerID, protocol.OperationCreate, fencedRequest)
	closeCtx, closeCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := oldLease.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("close old controller lease: %v", err)
	}
	closeCancel()
	oldLeaseClosed = true
	if _, err := oldClient.Create(ctx, newCreateRequest(t, oldEpoch, "closed-client")); !errors.Is(err, runner.ErrControllerAuthorityUnavailable) {
		t.Fatalf("closed issuer error=%v, want controller authority unavailable", err)
	}
	fencedBefore := backend.createCount()
	response = rawPost(t, ctx, rawClient, testServer.URL+protocol.PathCreate, fencedRequest)
	assertWireError(t, response, http.StatusConflict, protocol.CodeControllerFenced)
	backend.assertCreateDelta(t, fencedBefore, 0, 0)

	// A successor lease for the same provider advances the durable epoch and a
	// fresh proof issued by N+1 is accepted through the same private endpoint.
	successorLease, err := runner.AcquireControllerLease(ctx, runtimePool, providerID, func(loss error) {
		t.Errorf("successor controller lease lost unexpectedly: %v", loss)
	})
	if err != nil {
		t.Fatalf("acquire successor controller lease: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := successorLease.Close(closeCtx); err != nil {
			t.Errorf("close successor controller lease: %v", err)
		}
	})
	successorEpoch := successorLease.Fence().Epoch
	if successorEpoch != oldEpoch+1 {
		t.Fatalf("successor epoch=%d, want %d", successorEpoch, oldEpoch+1)
	}
	successorIssuer, err := runner.NewProtocolProofIssuer(successorLease)
	if err != nil {
		t.Fatalf("construct successor protocol proof issuer: %v", err)
	}
	successorClient := newProtocolClient(t, testServer.URL, clientTLS, providerID, successorIssuer)
	successorBefore := backend.createCount()
	if _, err := successorClient.Create(ctx, newCreateRequest(t, successorEpoch, "successor")); err != nil {
		t.Fatalf("successor create: %v", err)
	}
	backend.assertCreateDelta(t, successorBefore, 1, successorEpoch)

	if nonTLS13.Load() {
		t.Fatal("a request reached the server without TLS 1.3")
	}
	if strictSecurity != nil {
		assertStrictAttestationDriftRejected(t, ctx, adminPool, runtimePool, validatorPool, *strictSecurity)
	}
}

func openPool(t *testing.T, ctx context.Context, databaseURL, searchPath string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse PostgreSQL configuration: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = searchPath
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	return pool
}

type strictAttestationSecurity struct {
	private privateapi.PostgresProofValidatorSecurity
	backend dbsecurity.Config
}

func attestDatabaseLayout(t *testing.T, ctx context.Context, admin, runtime, validator *pgxpool.Pool, strictRoles bool) *strictAttestationSecurity {
	t.Helper()
	var database string
	var inRecovery bool
	var epochsTable, proofsTable, consumeFunction string
	if err := admin.QueryRow(ctx, `
		SELECT current_database(), pg_is_in_recovery(),
		       COALESCE(to_regclass('public.runner_controller_epochs')::TEXT,''),
		       COALESCE(to_regclass('public.runner_controller_proofs')::TEXT,''),
		       COALESCE(to_regprocedure($1)::TEXT,'')`, proofFunctionIdentity).Scan(
		&database, &inRecovery, &epochsTable, &proofsTable, &consumeFunction,
	); err != nil {
		t.Fatalf("attest proof database schema: %v", err)
	}
	if inRecovery || epochsTable == "" || proofsTable == "" || consumeFunction == "" {
		t.Fatalf("proof database is not a migrated writable primary: recovery=%t epochs=%q proofs=%q function=%q",
			inRecovery, epochsTable, proofsTable, consumeFunction)
	}
	for name, pool := range map[string]*pgxpool.Pool{"runtime": runtime, "validator": validator} {
		var connectedDatabase string
		if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&connectedDatabase); err != nil {
			t.Fatalf("read %s database identity: %v", name, err)
		}
		if connectedDatabase != database {
			t.Fatalf("%s database=%q, want %q", name, connectedDatabase, database)
		}
	}
	if !strictRoles {
		return nil
	}
	var runtimeRole, validatorRole string
	if err := runtime.QueryRow(ctx, `SELECT current_user`).Scan(&runtimeRole); err != nil {
		t.Fatalf("read runtime role: %v", err)
	}
	if err := validator.QueryRow(ctx, `SELECT current_user`).Scan(&validatorRole); err != nil {
		t.Fatalf("read validator role: %v", err)
	}
	if runtimeRole == validatorRole {
		t.Fatalf("strict runtime and validator URLs resolve to the same role %q", runtimeRole)
	}
	var runtimeCanConsume, runtimeCanDeleteProofs, runtimeCanDeleteConsumptions bool
	if err := runtime.QueryRow(ctx, `
		SELECT has_function_privilege(current_user,$1,'EXECUTE'),
		       has_table_privilege(current_user,'public.runner_controller_proofs','DELETE'),
		       has_table_privilege(current_user,'public.runner_controller_proof_consumptions','DELETE')`,
		proofFunctionIdentity).Scan(&runtimeCanConsume, &runtimeCanDeleteProofs, &runtimeCanDeleteConsumptions); err != nil {
		t.Fatalf("attest runtime proof privileges: %v", err)
	}
	if runtimeCanConsume || runtimeCanDeleteProofs || runtimeCanDeleteConsumptions {
		t.Fatalf("runtime proof privileges are overbroad: execute=%t delete_proofs=%t delete_consumptions=%t",
			runtimeCanConsume, runtimeCanDeleteProofs, runtimeCanDeleteConsumptions)
	}
	var validatorCanConsume, validatorCanReadProofs, validatorCanWriteProofs, validatorCanWriteConsumptions bool
	if err := validator.QueryRow(ctx, `
		SELECT has_function_privilege(current_user,$1,'EXECUTE'),
		       has_table_privilege(current_user,'public.runner_controller_proofs','SELECT'),
		       has_table_privilege(current_user,'public.runner_controller_proofs','INSERT,UPDATE,DELETE,TRUNCATE'),
		       has_table_privilege(current_user,'public.runner_controller_proof_consumptions','INSERT,UPDATE,DELETE,TRUNCATE')`,
		proofFunctionIdentity).Scan(
		&validatorCanConsume, &validatorCanReadProofs, &validatorCanWriteProofs, &validatorCanWriteConsumptions,
	); err != nil {
		t.Fatalf("attest validator proof privileges: %v", err)
	}
	if !validatorCanConsume || validatorCanReadProofs || validatorCanWriteProofs || validatorCanWriteConsumptions {
		t.Fatalf("validator privileges violate execute-only contract: execute=%t read=%t write_proofs=%t write_consumptions=%t",
			validatorCanConsume, validatorCanReadProofs, validatorCanWriteProofs, validatorCanWriteConsumptions)
	}
	security := deriveStrictAttestationSecurity(t, ctx, admin, runtimeRole, validatorRole)
	assertStrictAttestationsPass(t, ctx, runtime, validator, security)
	return &security
}

func deriveStrictAttestationSecurity(
	t *testing.T,
	ctx context.Context,
	admin *pgxpool.Pool,
	runtimeRole, validatorRole string,
) strictAttestationSecurity {
	t.Helper()
	var catalogOwner, ownerRole, migratorRole string
	if err := admin.QueryRow(ctx, `SELECT current_user`).Scan(&catalogOwner); err != nil {
		t.Fatalf("read catalog owner role: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT owner.rolname FROM pg_catalog.pg_namespace namespace
		JOIN pg_catalog.pg_roles owner ON owner.oid=namespace.nspowner
		WHERE namespace.nspname='public'`).Scan(&ownerRole); err != nil {
		t.Fatalf("read application owner role: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT member.rolname
		FROM pg_catalog.pg_auth_members membership
		JOIN pg_catalog.pg_roles granted ON granted.oid=membership.roleid
		JOIN pg_catalog.pg_roles member ON member.oid=membership.member
		WHERE granted.rolname=$1 AND membership.admin_option=FALSE
		  AND membership.inherit_option=FALSE AND membership.set_option=TRUE`, ownerRole).Scan(&migratorRole); err != nil {
		t.Fatalf("read migrator role: %v", err)
	}
	privateSecurity := privateapi.PostgresProofValidatorSecurity{
		CatalogOwnerRole: catalogOwner,
		OwnerRole:        ownerRole,
		MigratorRole:     migratorRole,
		RuntimeRole:      runtimeRole,
		ValidatorRole:    validatorRole,
	}
	return strictAttestationSecurity{
		private: privateSecurity,
		backend: dbsecurity.Config{
			CatalogOwnerRole: catalogOwner,
			OwnerRole:        ownerRole,
			MigratorRole:     migratorRole,
			RuntimeRole:      runtimeRole,
			ValidatorRole:    validatorRole,
			DedicatedCluster: true,
		},
	}
}

func assertStrictAttestationsPass(
	t *testing.T,
	ctx context.Context,
	runtime, validator *pgxpool.Pool,
	security strictAttestationSecurity,
) {
	t.Helper()
	if err := privateapi.VerifyPostgresProofValidatorSecurity(ctx, validator, security.private); err != nil {
		t.Fatalf("private validator security attestation: %v", err)
	}
	if err := dbsecurity.Verify(ctx, runtime, validator, security.backend); err != nil {
		t.Fatalf("backend security attestation: %v", err)
	}
}

func assertStrictAttestationDriftRejected(
	t *testing.T,
	ctx context.Context,
	admin, runtime, validator *pgxpool.Pool,
	security strictAttestationSecurity,
) {
	t.Helper()
	function := controlplanedb.ProofConsumerIdentity
	legacyFunction := controlplanedb.LegacyProofConsumerIdentity
	tests := []struct {
		name    string
		mutate  string
		restore string
	}{
		{
			name:    "owner role attributes",
			mutate:  "ALTER ROLE " + pgx.Identifier{security.private.OwnerRole}.Sanitize() + " LOGIN",
			restore: "ALTER ROLE " + pgx.Identifier{security.private.OwnerRole}.Sanitize() + " NOLOGIN",
		},
		{
			name:    "runtime membership",
			mutate:  "GRANT pg_monitor TO " + pgx.Identifier{security.private.RuntimeRole}.Sanitize(),
			restore: "REVOKE pg_monitor FROM " + pgx.Identifier{security.private.RuntimeRole}.Sanitize(),
		},
		{
			name:    "public database connect",
			mutate:  "GRANT CONNECT ON DATABASE " + pgx.Identifier{currentDatabase(t, ctx, admin)}.Sanitize() + " TO PUBLIC",
			restore: "REVOKE CONNECT ON DATABASE " + pgx.Identifier{currentDatabase(t, ctx, admin)}.Sanitize() + " FROM PUBLIC",
		},
		{
			name:    "public schema create",
			mutate:  "GRANT CREATE ON SCHEMA public TO PUBLIC",
			restore: "REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		},
		{
			name: "validator column authority",
			mutate: "GRANT SELECT (token_hash) ON TABLE public.runner_controller_proofs TO " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
			restore: "REVOKE SELECT (token_hash) ON TABLE public.runner_controller_proofs FROM " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
		},
		{
			name: "validator function grant option",
			mutate: "GRANT EXECUTE ON FUNCTION " + function + " TO " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize() + " WITH GRANT OPTION",
			restore: "REVOKE GRANT OPTION FOR EXECUTE ON FUNCTION " + function + " FROM " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
		},
		{
			name:    "public proof consumer execution",
			mutate:  "GRANT EXECUTE ON FUNCTION " + function + " TO PUBLIC",
			restore: "REVOKE EXECUTE ON FUNCTION " + function + " FROM PUBLIC",
		},
		{
			name: "legacy proof consumer",
			mutate: `CREATE FUNCTION ` + legacyFunction + ` RETURNS TEXT
				LANGUAGE SQL AS 'SELECT ''unauthorized''::text'`,
			restore: "DROP FUNCTION " + legacyFunction,
		},
		{
			name: "validator advisory execution",
			mutate: "GRANT EXECUTE ON FUNCTION pg_catalog.pg_advisory_lock(bigint) TO " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
			restore: "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_advisory_lock(bigint) FROM " +
				pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
		},
		{
			name: "owner default table privilege",
			mutate: "ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{security.private.OwnerRole}.Sanitize() +
				" IN SCHEMA public GRANT SELECT ON TABLES TO " + pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
			restore: "ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{security.private.OwnerRole}.Sanitize() +
				" IN SCHEMA public REVOKE SELECT ON TABLES FROM " + pgx.Identifier{security.private.ValidatorRole}.Sanitize(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertOneStrictDriftRejected(t, ctx, admin, runtime, validator, security, test.mutate, test.restore)
		})
	}

	t.Run("v2 body replacement", func(t *testing.T) {
		var originalDefinition string
		if err := admin.QueryRow(ctx, `SELECT pg_catalog.pg_get_functiondef(pg_catalog.to_regprocedure($1))`, function).
			Scan(&originalDefinition); err != nil {
			t.Fatalf("read original proof consumer definition: %v", err)
		}
		unsafeDefinition := `CREATE OR REPLACE FUNCTION public.consume_runner_controller_proof_v2(
			p_provider_id TEXT, p_epoch BIGINT, p_lease_id UUID, p_proof_id UUID,
			p_operation TEXT, p_request_digest BYTEA, p_effect_deadline TIMESTAMPTZ,
			p_issued_at TIMESTAMPTZ, p_expires_at TIMESTAMPTZ, p_issuer_uri TEXT,
			p_audience_uri TEXT, p_token_hash BYTEA
		) RETURNS TEXT LANGUAGE plpgsql SECURITY DEFINER SET search_path TO pg_catalog
		AS $function$ BEGIN RETURN 'accepted'; END $function$`
		assertOneStrictDriftRejected(t, ctx, admin, runtime, validator, security, unsafeDefinition, originalDefinition)
	})
}

func assertOneStrictDriftRejected(
	t *testing.T,
	ctx context.Context,
	admin, runtime, validator *pgxpool.Pool,
	security strictAttestationSecurity,
	mutate, restore string,
) {
	t.Helper()
	if _, err := admin.Exec(ctx, mutate); err != nil {
		t.Fatalf("apply security drift: %v", err)
	}
	privateErr := privateapi.VerifyPostgresProofValidatorSecurity(ctx, validator, security.private)
	backendErr := dbsecurity.Verify(ctx, runtime, validator, security.backend)
	if _, err := admin.Exec(ctx, restore); err != nil {
		t.Fatalf("restore security drift: %v", err)
	}
	if privateErr == nil {
		t.Error("private validator attestation accepted security drift")
	}
	if backendErr == nil {
		t.Error("backend attestation accepted security drift")
	}
	assertStrictAttestationsPass(t, ctx, runtime, validator, security)
}

func currentDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var database string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("read current database: %v", err)
	}
	return database
}

func newProtocolClient(t *testing.T, baseURL string, tlsConfig *tls.Config, providerID string, issuer protocol.AuthorityProofIssuer) *protocol.Client {
	t.Helper()
	client, err := protocol.NewClient(protocol.ClientConfig{
		BaseURL: baseURL, TLSConfig: tlsConfig, ExpectedServerURI: serverURI,
		ProviderID: providerID, AuthorityIssuerURI: clientURI, ProofIssuer: issuer,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("construct protocol client: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func newCreateRequest(t *testing.T, epoch uint64, keySuffix string) protocol.CreateRequest {
	t.Helper()
	sessionID := randomUUID(t)
	generation := uint64(1)
	return protocol.CreateRequest{
		Schema: protocol.Schema, ControllerEpoch: epoch,
		EffectDeadline: canonicalDatabaseTime(t, time.Now().UTC().Add(30*time.Second)),
		AllocationID:   proxmox.AllocationIDForSession(sessionID, generation),
		SessionID:      sessionID, Generation: generation, UserID: randomUUID(t),
		CatalogGeneration: 1, ProblemID: "pod-crash",
		ProblemRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResourceProfile: "small",
		ExpiresAt:       canonicalTime(t, time.Now().UTC().Add(time.Hour)),
		IdempotencyKey:  "e2e-" + keySuffix,
	}
}

func issueProof(
	t *testing.T,
	ctx context.Context,
	issuer protocol.AuthorityProofIssuer,
	providerID, operation string,
	request protocol.CreateRequest,
) protocol.AuthorityProof {
	t.Helper()
	request.AuthorityProof = protocol.AuthorityProof{}
	digest, err := protocol.RequestDigest(providerID, request)
	if err != nil {
		t.Fatalf("digest request: %v", err)
	}
	deadline, err := protocol.ParseCanonicalDatabaseUTC(request.EffectDeadline)
	if err != nil {
		t.Fatalf("parse request deadline: %v", err)
	}
	proof, err := issuer.IssueAuthorityProof(ctx, protocol.AuthorityProofRequest{
		Operation: operation, RequestDigest: digest, EffectDeadline: deadline,
		IssuerURI: clientURI, AudienceURI: serverURI,
	})
	if err != nil {
		t.Fatalf("issue authority proof: %v", err)
	}
	return proof
}

type rawResponse struct {
	status int
	body   []byte
}

func rawPost(t *testing.T, ctx context.Context, client *http.Client, endpoint string, value any) rawResponse {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw request: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("construct raw request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send raw request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		t.Fatalf("read raw response: %v", err)
	}
	return rawResponse{status: response.StatusCode, body: body}
}

func assertSuccess(t *testing.T, response rawResponse) {
	t.Helper()
	if response.status != http.StatusOK {
		t.Fatalf("response status=%d body=%s", response.status, response.body)
	}
	var output protocol.AllocationResponse
	if err := protocol.DecodeStrict(response.body, &output); err != nil || output.Schema != protocol.Schema {
		t.Fatalf("invalid success response: body=%s err=%v", response.body, err)
	}
}

func assertWireError(t *testing.T, response rawResponse, status int, code protocol.ErrorCode) {
	t.Helper()
	if response.status != status {
		t.Fatalf("response status=%d body=%s, want %d", response.status, response.body, status)
	}
	var output protocol.ErrorResponse
	if err := protocol.DecodeStrict(response.body, &output); err != nil {
		t.Fatalf("decode error response: body=%s err=%v", response.body, err)
	}
	if output.Schema != protocol.ErrorSchema || output.Error.Code != code {
		t.Fatalf("error response=%+v, want code %s", output, code)
	}
}

type recordingBackend struct {
	mu      sync.Mutex
	creates []recordedCreate
}

type recordedCreate struct {
	epoch   uint64
	command privateapi.CreateCommand
}

func (b *recordingBackend) ActivateController(context.Context, privateapi.VerifiedAuthority) error {
	return nil
}

func (b *recordingBackend) Create(_ context.Context, authority privateapi.VerifiedAuthority, command privateapi.CreateCommand) (privateapi.AllocationState, error) {
	b.mu.Lock()
	b.creates = append(b.creates, recordedCreate{epoch: authority.Epoch(), command: command})
	b.mu.Unlock()
	return privateapi.AllocationState{
		AllocationID: command.AllocationID, SessionID: command.SessionID,
		Generation: command.Generation, CatalogGeneration: command.CatalogGeneration,
		DesiredState: "active", Phase: "running", ObservedState: "running",
		ExpiresAt: command.ExpiresAt,
	}, nil
}

func (b *recordingBackend) Get(context.Context, privateapi.VerifiedAuthority, privateapi.AllocationIdentity) (privateapi.AllocationState, error) {
	return privateapi.AllocationState{}, privateapi.ErrUnsupported
}

func (b *recordingBackend) Destroy(context.Context, privateapi.VerifiedAuthority, privateapi.DestroyCommand) (privateapi.AllocationState, error) {
	return privateapi.AllocationState{}, privateapi.ErrUnsupported
}

func (b *recordingBackend) createCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.creates)
}

func (b *recordingBackend) assertCreateDelta(t *testing.T, before, delta int, epoch uint64) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.creates) != before+delta {
		t.Fatalf("backend create calls=%d, want %d", len(b.creates), before+delta)
	}
	if delta == 1 && b.creates[len(b.creates)-1].epoch != epoch {
		t.Fatalf("backend authority epoch=%d, want %d", b.creates[len(b.creates)-1].epoch, epoch)
	}
}

type ephemeralPKI struct {
	roots             *x509.CertPool
	rootCertificate   *x509.Certificate
	serverCertificate tls.Certificate
	clientCertificate tls.Certificate
}

func newEphemeralPKI(t *testing.T) ephemeralPKI {
	t.Helper()
	now := time.Now().UTC()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: randomSerial(t), Subject: pkix.Name{CommonName: "runner-proof-e2e-ca"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("issue CA certificate: %v", err)
	}
	rootCertificate, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCertificate)
	return ephemeralPKI{
		roots: roots, rootCertificate: rootCertificate,
		serverCertificate: issueLeaf(t, rootCertificate, rootKey, now,
			[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{serverName}, []*url.URL{mustURL(t, serverURI)}),
		clientCertificate: issueLeaf(t, rootCertificate, rootKey, now,
			[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, []*url.URL{mustURL(t, clientURI)}),
	}
}

func issueLeaf(
	t *testing.T,
	issuer *x509.Certificate,
	issuerKey *ecdsa.PrivateKey,
	now time.Time,
	usages []x509.ExtKeyUsage,
	dnsNames []string,
	uriNames []*url.URL,
) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: randomSerial(t), Subject: pkix.Name{CommonName: "ignored"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
		DNSNames: dnsNames, URIs: uriNames,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("issue leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ephemeral leaf key: %v", err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	clear(keyDER)
	if err != nil {
		t.Fatalf("construct leaf key pair: %v", err)
	}
	return certificate
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("generate certificate serial: %v", err)
	}
	return serial
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func randomHex(t *testing.T, size int) string {
	t.Helper()
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate random provider suffix: %v", err)
	}
	return hex.EncodeToString(value)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse test URI: %v", err)
	}
	return parsed
}

func canonicalDatabaseTime(t *testing.T, value time.Time) string {
	t.Helper()
	encoded, err := protocol.FormatCanonicalDatabaseUTC(value.Truncate(time.Microsecond))
	if err != nil {
		t.Fatalf("format canonical database time: %v", err)
	}
	return encoded
}

func canonicalTime(t *testing.T, value time.Time) string {
	t.Helper()
	encoded := value.UTC().Format(time.RFC3339Nano)
	if _, err := protocol.ParseCanonicalUTC(encoded); err != nil {
		t.Fatalf("format canonical time: %v", err)
	}
	return encoded
}
