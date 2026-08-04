# ADR 002: Problem artifact catalog and publication boundary

- Status: Accepted for the development runtime
- Decision type: Durable catalog ledger and local integrity CAS implemented; public signed-artifact trust gate open
- Scope: Problem metadata, runtime files, images, catalog publication, session selection, and recovery

## Context

A problem revision influences both the environment created for a learner and the
evidence used to grade it. Reading `problem.yaml`, setup, verification, hint, and
image inputs from a mutable checkout at different times could bind one revision to
mixed bytes. Publishing a new in-memory bundle independently of PostgreSQL could also
let a fresh session reserve a revision that the runtime does not hold, or the reverse.

Content identity, publication order, and approval provenance are different claims.
The local `problems/` checkout may contain dirty or untracked files. A digest computed
from a stable capture identifies those captured bytes, but does not prove repository
origin, review, publisher approval, reproducibility, revocation state, or suitability
for public execution.

## Decision implemented

### Immutable development snapshot

The strict runtime loader captures every runtime-relevant file through directory file
descriptors, rejects symbolic links, hard links, special files, and oversized files,
and requires two identical full captures. It resolves the provider image to an
immutable content ID and includes that ID and `images.lock` in the problem revision.
Script problems require a non-empty verifier. Bundles are deep-copied and an activated
process retains exact revisions needed by its sessions.

The current source trust value is `development_checkout`. It is intentionally
insufficient for public admission.

### Append-only PostgreSQL publication ledger

PostgreSQL is the durable catalog publication authority:

- `problem_catalog_publications` stores a monotonic generation chain and canonical
  candidate digest;
- `problem_catalog_entries` stores the exact problem metadata selection for every
  generation;
- the singleton `problem_catalog_head` names the active generation and advances by
  compare-and-swap;
- database triggers seal completed publications against update, delete, and truncate,
  enforce chain/head shape, and require complete entry counts;
- `problems.catalog_active` remains the public/latest projection. Inactive rows in
  `problems` are FK-preserving identity rows, not the revision ledger.

An explicit administrative Sync performs the following while new-start admission is
closed:

1. `BuildCandidate` captures and validates an immutable candidate without making it
   resolvable.
2. The coordinator acquires the global PostgreSQL publication lease, compares the
   persisted head with its process-local head, and prepares a generation-bound loader
   activation.
3. One transaction appends `head+1`, stores every generation entry, updates the latest
   projection, and advances the head by CAS.
4. Only after the database commit is proven does the loader activate the same candidate
   and reopen admission.

Sync of an unchanged current head is an idempotent no-op. Republishing an older
candidate digest is rejected as rollback.

### Canonical runtime artifact and local integrity CAS

Every strict candidate is encoded as a schema-versioned canonical runtime artifact.
It contains the raw `problem.yaml`, `images.lock`, optional setup, verifier, and hint
files (including the difference between missing and present-empty), the immutable
runtime image content ID, source-trust class, legacy problem revision, and the full
runtime problem projection. Decoding verifies the SHA-256 artifact reference, fixed
record order and exact EOF, allocation bounds, canonical re-encoding, source
semantics, image policy, and the legacy revision.

Before publication, every artifact is installed and read back from the configured
filesystem content-addressed store. The store uses a private owner-only root,
descriptor-relative no-follow traversal, regular-file/owner/mode/hard-link checks,
bounded reads, exclusive temporary files, file and directory `fsync`, and atomic
no-replace installation. An existing corrupt or conflicting object is never
overwritten. The mutable checkout and artifact store must be disjoint directory
trees.

`problem_artifacts` append-only rows bind each `(problem_id, problem_revision)` to
the exact artifact digest schema, digest, media type, size, and `source_trust`.
Catalog entries require that binding, and fresh sessions require an artifact-bound
current head. Existing version-11 publications remain explicitly unmapped until an
exact checkout is captured, installed, read back, and shown to reproduce the legacy
head and projection; migration code never invents their bytes.

### Safe startup semantics

Startup is not a publication mechanism for changed content:

- when no persisted head exists, startup prepares and reads back the candidate CAS
  objects before taking the admission fence and publication lease, then rechecks the
  head and may bootstrap generation 1 after checking that no unreconciled legacy
  active session lacks catalog provenance;
- if another process wins that bootstrap race, the loser discards its candidate as a
  publication intent and adopts the winner's persisted head instead of appending
  generation 2;
- when an artifact-bound head exists, startup reconstructs it from PostgreSQL and
  the CAS without reading the checkout. A changed or missing checkout is therefore
  irrelevant to ordinary restart adoption;
- a missing, corrupt, conflicting, or projection-divergent referenced artifact fails
  startup closed and does not append, replace, or roll back the ledger;
- a legacy version-11 head is the only startup path that reads the checkout, solely
  for exact one-time artifact binding;
- all later content changes require explicit admin Sync.

This prevents a restart, deployment mistake, or mutable checkout from silently
becoming a catalog publication.

### Ambiguous commit reconciliation

Known non-commit outcomes leave the previous generation active. If COMMIT
acknowledgement is ambiguous, the coordinator discards that database connection,
reacquires the global publication lease on a fresh connection, and reads both the
head and the candidate's publication:

- if the exact candidate was not recorded and the old head is unchanged, the result is
  a confirmed non-application and admission remains healthy;
- if the exact generation, digest, entry count, complete projection, and current head
  all match, the coordinator activates that exact candidate and converges;
- any missing, partial, divergent, or superseded observation poisons new admission
  rather than guessing.

Existing exact-revision verification, reset, terminal, cleanup, and recovery paths
remain available when only new admission is poisoned.

The version-11 artifact-binding transaction follows the same rule: an ambiguous
connection is discarded, and a fresh lease either proves the complete exact binding
or safely retries the idempotent binding. Partial, changed-head, or divergent
observations close new admission.

### Durable session selection

A fresh start acquires an active-catalog admission lease and receives a
`CatalogSelection{Generation, ProblemRef}` from the current PostgreSQL head. The
durable reservation transaction revalidates that exact generation/problem/revision as
the current head and stores it in `sessions.catalog_generation`. The lease remains held
through reservation commit, so a start is wholly before or after a publication.

Reset preserves the original catalog selection. An exact idempotent replay resolves
the selection already stored in its durable reservation rather than consulting the
new head. Thus publication provenance survives later problem update or retirement.

## Guarantees and explicit limits

The implementation now provides a durable, append-only metadata ledger, canonical
runtime artifact format, local filesystem integrity CAS, immutable revision-to-
artifact binding, globally ordered head CAS, checkout-free exact startup adoption,
historical runtime recovery, ambiguous-commit reconciliation, and durable session-to-
catalog-generation binding. It closes the database/in-memory publication race for
the current controller topology and preserves historical metadata and runtime bytes
across an ordinary process restart when the configured CAS volume remains available.

It does **not** make the service public-ready:

- the implemented CAS is a local integrity and recovery store, not a signed release
  registry, replicated availability service, backup policy, or public trust root;
- losing or rolling back both PostgreSQL and the CAS volume can still make referenced
  artifacts unavailable; deployment backup/restore evidence is not implemented;
- two identical filesystem captures are not a transaction, signature, or approval;
- the Unix no-follow implementation does not by itself exclude every mount-boundary
  or intermediate-path attack;
- image digest pinning establishes selected content identity, not publisher/build
  provenance or target-platform approval;
- approval, revocation, expiry, and trusted publisher policy are not implemented;
- no public-capable Proxmox/cloud provider or external trusted verifier is established
  by this decision.

## Remaining public artifact trust requirement

Before a public Proxmox or cloud provider is adoptable, the canonical artifacts must
be promoted through a signed, approved, revocable release and distribution layer.
That layer may reuse the implemented digest/CAS format, but a canonical signed
manifest must bind at least:

- schema/media type and algorithm-qualified artifact digest;
- problem ID, exact file inventory, modes, sizes, and individual digests;
- source repository identity, full commit and tree, parent release and submodule
  identity where applicable;
- setup and verifier artifact digests;
- VM/runtime/workload OCI manifest or index digests and target platform;
- verifier worker image and toolchain digest;
- reproducible build provenance with trusted builder/workflow/material identities;
- a separate approval attestation with policy version, approver identity or quorum,
  validity, and the exact artifact digest as subject;
- approved or revoked state, revocation epoch/reason, admission time, and expiry.

The released artifact must be verified before extraction, stored and fetched by
digest, backed up or replicated to the deployment's recovery objective, and rechecked
for approval and revocation at create and verify time. Startup recovery must load
every artifact referenced by an active durable reservation before opening admission.
ADR 001's verification receipt must bind the exact problem and verifier artifact
digests.

## Required public acceptance evidence

The public artifact gate remains open until tests prove all of the following against
the deployed artifact service and provider:

1. A clean recursive clone/build reproduces the released artifact digest; dirty,
   untracked, wrong-submodule, wrong-repository, or wrong-workflow inputs are rejected.
2. Bit changes, missing or extra files, path traversal, name collisions, symbolic and
   hard links, special files, mount escapes, oversized inputs, and mode mismatches are
   rejected during safe extraction and inventory verification.
3. Wrong, untrusted, expired, or revoked signatures and approvals fail closed. Build
   provenance without separate approval, or approval for another subject, is
   insufficient.
4. A problem revoked between create and verify cannot produce an accepted grade, and
   rollback to an older catalog generation is rejected.
5. Controller restart, artifact-service outage, and backup restoration retain every
   active exact artifact. Two controllers cannot publish or admit different bytes for
   one durable generation.
6. Removed problems disappear from new admission while existing exact-selection
   sessions can complete bounded cleanup or policy-approved recovery.
7. Mutable, unsigned, wrong-platform, or unapproved dependency images are rejected.

## Consequences

The ledger plus local CAS are a durable development-runtime correctness boundary that
supports provider-neutral Runner development and checkout-free restart recovery. They
are not a signed release registry or a public trust root. Public readiness still
requires the signed provenance/approval/revocation layer above, ADR 001's trusted
verifier topology, and live Proxmox isolation, network, quota, failure, and cleanup
evidence.
