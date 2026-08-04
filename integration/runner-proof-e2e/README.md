# Runner authority proof cross-repository E2E

This independent Go module verifies the complete private Runner authority path
without adding the private infrastructure module to the production backend:

```text
ControllerLease -> ProtocolProofIssuer -> public protocol Client
  -> TLS 1.3 mTLS -> private Runner Server
  -> PostgreSQL proof consumption -> fake lifecycle backend
```

The test never runs migrations and uses a cryptographically unique provider ID,
so its lease and append-only proof rows cannot collide with another run. Point it
only at a disposable PostgreSQL database whose `public` schema is already at the
latest backend migration.

Required environment:

```text
RUNNER_PROOF_E2E_DATABASE_URL
```

For the strict production-like role split, set both variables below. Supplying
only one skips the test rather than silently mixing security roles:

```text
RUNNER_PROOF_E2E_RUNTIME_DATABASE_URL
RUNNER_PROOF_E2E_VALIDATOR_DATABASE_URL
```

No certificate or private key is written to disk. The test generates an
ephemeral CA and leaf keys in memory for each run.
