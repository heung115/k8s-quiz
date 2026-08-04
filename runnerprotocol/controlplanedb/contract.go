// Package controlplanedb defines the public, non-secret PostgreSQL security
// contract shared by the Control Plane hardener and private Runner startup
// attestation. It contains no credentials, endpoints, or deployment role names.
package controlplanedb

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	Schema = "public"

	ProofConsumerIdentity       = "public.consume_runner_controller_proof_v2(text,bigint,uuid,uuid,text,bytea,timestamp with time zone,timestamp with time zone,timestamp with time zone,text,text,bytea)"
	LegacyProofConsumerIdentity = "public.consume_runner_controller_proof(text,bigint,uuid,uuid,text,bytea,timestamp with time zone,timestamp with time zone,timestamp with time zone,text,text,bytea)"

	// ProofConsumerBodySHA256 is SHA-256 over strings.Fields(prosrc), joined by
	// one ASCII space. It pins migration 017's post-lock time revalidation and
	// prevents a legacy body from being installed behind the v2 function name.
	ProofConsumerBodySHA256 = "63cea0fc9740824883d8b502858e0638629eb377362528c0e6d42383afc77f02"
)

type TablePolicy struct {
	SelectAll     bool
	SelectColumns []string
	InsertColumns []string
	UpdateColumns []string
	DeleteRows    bool
}

var applicationTables = []string{
	"schema_migrations",
	"users",
	"problems",
	"attempts",
	"refresh_tokens",
	"sessions",
	"runner_allocations",
	"runner_operations",
	"session_events",
	"runner_controller_epochs",
	"problem_catalog_publications",
	"problem_catalog_entries",
	"problem_catalog_head",
	"problem_artifacts",
	"runner_controller_proofs",
	"runner_controller_proof_consumptions",
}

var applicationFunctions = []string{
	"public.reject_problem_catalog_mutation()",
	"public.guard_problem_catalog_publication_insert()",
	"public.guard_problem_catalog_entry_insert()",
	"public.guard_problem_catalog_head()",
	"public.validate_problem_catalog_publication_complete()",
	"public.guard_session_catalog_selection()",
	"public.guard_problem_artifact_insert()",
	ProofConsumerIdentity,
}

var triggerFunctions = []string{
	"public.reject_problem_catalog_mutation()",
	"public.guard_problem_catalog_publication_insert()",
	"public.guard_problem_catalog_entry_insert()",
	"public.guard_problem_catalog_head()",
	"public.validate_problem_catalog_publication_complete()",
	"public.guard_session_catalog_selection()",
	"public.guard_problem_artifact_insert()",
}

var runtimeAdvisoryFunctions = map[string]bool{
	"pg_advisory_lock(bigint)":             true,
	"pg_advisory_unlock(bigint)":           true,
	"pg_advisory_xact_lock(bigint)":        true,
	"pg_advisory_xact_lock_shared(bigint)": true,
	"pg_try_advisory_lock(bigint)":         true,
}

var runtimeExtensionFunctions = map[string]bool{
	"public.gen_random_uuid()": true,
}

var runtimePolicies = map[string]TablePolicy{
	"schema_migrations": {SelectAll: true},
	"users": {
		SelectAll:     true,
		InsertColumns: []string{"github_id", "username", "email", "avatar_url", "role", "created_at", "updated_at"},
		UpdateColumns: []string{"username", "email", "avatar_url", "role", "updated_at"},
	},
	"problems": {
		SelectAll: true, DeleteRows: true,
		InsertColumns: []string{"id", "revision", "catalog_active", "title", "description", "category", "difficulty", "type", "timeout_minutes", "verify_type", "base_image", "image", "choices", "correct_choice", "hint", "grading_prompt", "created_at", "updated_at"},
		UpdateColumns: []string{"revision", "catalog_active", "title", "description", "category", "difficulty", "type", "timeout_minutes", "verify_type", "base_image", "image", "choices", "correct_choice", "hint", "grading_prompt", "updated_at"},
	},
	"attempts": {
		SelectAll:     true,
		InsertColumns: []string{"id", "user_id", "problem_id", "status", "container_id", "started_at", "session_id", "generation"},
		UpdateColumns: []string{"status", "finished_at", "duration_seconds", "verify_log"},
	},
	"refresh_tokens": {
		SelectAll: true, DeleteRows: true,
		InsertColumns: []string{"user_id", "token_hash", "expires_at", "family_id"},
		UpdateColumns: []string{"used"},
	},
	"sessions": {
		SelectAll:     true,
		InsertColumns: []string{"id", "user_id", "problem_id", "problem_revision", "catalog_generation", "current_generation", "state", "desired_state", "queued_at", "expires_at"},
		UpdateColumns: []string{"current_generation", "state", "desired_state", "started_at", "finished_at", "lock_version", "updated_at"},
	},
	"runner_allocations": {
		SelectAll:     true,
		InsertColumns: []string{"id", "session_id", "generation", "provider_kind", "provider_id", "resource_profile", "desired_state", "observed_state", "expires_at", "last_event_sequence"},
		UpdateColumns: []string{"external_id", "desired_state", "observed_state", "last_observed_at", "failure_code", "failure_message", "last_event_sequence", "lock_version", "updated_at"},
	},
	"runner_operations": {
		SelectAll:     true,
		InsertColumns: []string{"allocation_id", "provider_id", "kind", "idempotency_key", "request_hash", "state", "started_at"},
		UpdateColumns: []string{"state", "error_code", "error_message", "result_metadata", "attempt_count", "next_attempt_at", "lease_token", "lease_owner", "lease_expires_at", "started_at", "completed_at", "updated_at"},
	},
	"session_events": {
		SelectAll:     true,
		InsertColumns: []string{"allocation_id", "sequence", "event_type", "reason_code", "message", "sanitized_payload", "created_at", "event_key"},
	},
	"runner_controller_epochs": {
		SelectAll:     true,
		InsertColumns: []string{"provider_id", "epoch", "lease_id", "lease_backend_pid", "lease_backend_start", "lease_advisory_key", "updated_at"},
		UpdateColumns: []string{"epoch", "lease_id", "lease_backend_pid", "lease_backend_start", "lease_advisory_key", "updated_at"},
	},
	"problem_catalog_publications": {
		SelectAll:     true,
		InsertColumns: []string{"generation", "previous_generation", "digest_schema", "candidate_digest", "entry_count"},
	},
	"problem_catalog_entries": {
		SelectAll:     true,
		InsertColumns: []string{"catalog_generation", "problem_id", "problem_revision", "title", "description", "category", "difficulty", "type", "timeout_minutes", "verify_type", "base_image", "image", "choices", "correct_choice", "hint", "grading_prompt"},
	},
	"problem_catalog_head": {
		SelectAll:     true,
		InsertColumns: []string{"singleton", "catalog_generation"},
		UpdateColumns: []string{"catalog_generation"},
	},
	"problem_artifacts": {
		SelectAll:     true,
		InsertColumns: []string{"problem_id", "problem_revision", "artifact_digest_schema", "artifact_digest", "artifact_media_type", "artifact_size", "source_trust"},
	},
	"runner_controller_proofs": {
		SelectColumns: []string{"issued_at", "expires_at"},
		InsertColumns: []string{"provider_id", "epoch", "lease_id", "proof_id", "operation", "request_digest", "effect_deadline", "issuer_uri", "audience_uri", "token_hash", "issued_at", "expires_at"},
	},
	"runner_controller_proof_consumptions": {},
}

func ApplicationTables() []string    { return append([]string(nil), applicationTables...) }
func ApplicationFunctions() []string { return append([]string(nil), applicationFunctions...) }
func TriggerFunctions() []string     { return append([]string(nil), triggerFunctions...) }

func RuntimeAdvisoryFunctions() map[string]bool  { return cloneBoolMap(runtimeAdvisoryFunctions) }
func RuntimeExtensionFunctions() map[string]bool { return cloneBoolMap(runtimeExtensionFunctions) }

func RuntimePolicies() map[string]TablePolicy {
	result := make(map[string]TablePolicy, len(runtimePolicies))
	for table, policy := range runtimePolicies {
		policy.SelectColumns = append([]string(nil), policy.SelectColumns...)
		policy.InsertColumns = append([]string(nil), policy.InsertColumns...)
		policy.UpdateColumns = append([]string(nil), policy.UpdateColumns...)
		result[table] = policy
	}
	return result
}

func ProofConsumerBodyMatches(body string) bool {
	normalized := strings.Join(strings.Fields(body), " ")
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:]) == ProofConsumerBodySHA256
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
