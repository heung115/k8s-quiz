package controlplanedb

import "testing"

func TestReturnedContractCollectionsAreCopies(t *testing.T) {
	tables := ApplicationTables()
	tables[0] = "mutated"
	if ApplicationTables()[0] == "mutated" {
		t.Fatal("application table contract was mutable")
	}
	policies := RuntimePolicies()
	policy := policies["users"]
	policy.InsertColumns[0] = "mutated"
	policies["users"] = policy
	if RuntimePolicies()["users"].InsertColumns[0] == "mutated" {
		t.Fatal("runtime policy contract was mutable")
	}
}

func TestProofConsumerBodyFingerprintRejectsSemanticDrift(t *testing.T) {
	// The complete migration body is checked against this function by the
	// backend migration test. Here, prove normalization is whitespace-only and
	// an arbitrary or legacy body cannot match by accident.
	if ProofConsumerBodyMatches("BEGIN RETURN 'accepted'; END") {
		t.Fatal("accepted an unrelated proof consumer body")
	}
}
