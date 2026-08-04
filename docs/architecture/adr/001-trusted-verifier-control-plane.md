# ADR 001: Trusted verifier control plane

- Status: Proposed
- Decision type: Public acceptance gate
- Scope: Public Proxmox/KVM and future cloud Runner providers

## Context

A learner is expected to control a root shell in the exercise environment. Root in
that environment can replace the shell, `kubectl`, PATH entries, kubeconfig,
certificates, API endpoints, and local k3s components. It can therefore forge the
inputs and exit status of a verifier executed inside the same guest. Keeping the
approved verifier script outside the guest until execution does not repair this
boundary: the script would still consume learner-controlled tools and evidence.

Moving the verifier process outside the Session VM is also insufficient when it uses
`kubectl` against a Kubernetes API owned by learner root. A learner-controlled API can
return a fabricated cluster state to an otherwise immutable external verifier.
Separate-kernel KVM isolation protects the host boundary, but does not by itself make
grading evidence trustworthy.

The current Local Docker adapter executes verification inside the learner environment
and is development-only. It is not evidence for public verification safety.

## Decision

Public verification must trust neither the learner-owned operating system nor a
learner-owned Kubernetes API. The public execution topology must use one of these
models:

1. A trusted per-session Kubernetes control plane/API outside learner-root authority,
   with the learner shell and workload node separated from that control plane.
2. An environment in which the learner does not receive VM root and cannot modify the
   Kubernetes API server, its identity material, or the verifier's observation path.

The exact model remains an implementation decision. Until one model is implemented
and passes the live security tests in this ADR, the `trusted_external` verifier
capability is unavailable and must fail closed. A home Proxmox public provider is
blocked on this topology; creating a disposable KVM VM alone does not open the gate.

Verification runs in a separate, unprivileged verifier worker with all of the
following properties:

- The worker image and verifier artifact are immutable and digest-pinned against an
  operator-approved catalog.
- The worker has a read-only root filesystem, no host mounts, and no Proxmox, cloud,
  Runner-management, PostgreSQL, or Control Plane credentials.
- Its network policy permits only the exact trusted API endpoint for the allocation,
  plus narrowly required infrastructure such as DNS. General Internet, management,
  service, home, metadata, and other-session networks are denied.
- Each operation receives a short-lived, least-privilege credential bound to the
  exact allocation and generation. Read-only observation is the default; narrowly
  scoped impersonation checks may be allowed for an RBAC problem. The credential,
  endpoint authority, and private key must never be written into or exposed to the
  learner guest.
- The worker is bounded by CPU, memory, PIDs, output size, absolute deadline, and
  cancellation. Completion or cancellation revokes the operation credential.
- Verification is observation-only. It must not depend on guest execution, a
  learner-created diagnostic pod, or a command supplied by the browser, Control Plane,
  or learner environment.

The verifier returns a structured, authenticated receipt. Before accepting a grade,
the Runner and Control Plane must validate that the receipt binds exactly to:

- schema version and verifier execution identity
- operation and idempotency identity
- session ID, generation, and allocation ID
- provider and current controller fence/epoch
- approved problem revision and verifier artifact digest
- trusted API audience or endpoint identity
- start time, finish time, and absolute deadline
- verdict (`passed` or `failed`) and evidence digest

The receipt must be authenticated by the trusted verifier service identity or a
dedicated signing key unavailable to the learner. Any missing, stale, expired,
cross-allocation, cross-generation, unexpected-revision, invalid-signature, or
fence-mismatched receipt is an infrastructure/security failure, never a legitimate
grade. A late result cannot mutate a newer generation.

Browser-visible feedback is not raw verifier output. The public result contains only
an allowlisted feedback code and bounded, schema-validated parameters whose values
come from the approved problem manifest where applicable. It is valid UTF-8, removes
control characters, and fits the public response bound. Raw stdout, stderr, API
responses, addresses, credentials, certificates, and provider diagnostics are private
diagnostics. They may be stored in access-controlled telemetry with a bounded
retention period and referenced by an opaque diagnostic ID, but must never enter
attempt logs, lifecycle events, REST responses, or browser WebSocket messages.

## Rejected alternatives

### Execute the approved verifier in the learner guest

Rejected because learner root controls the interpreter, tools, kubeconfig,
certificates, API endpoint, and observed cluster objects. Approved script bytes do
not establish trusted execution or evidence.

### Run external `kubectl` against a learner-owned Kubernetes API

Rejected because separating the client binary does not authenticate the truth of a
server controlled by the learner. The learner can replace or reconfigure the API and
fabricate responses.

### Protect only the `kubectl` binary or PATH

Rejected because root can alter the shell, libraries, kubeconfig, certificates,
network routing, API endpoint, k3s server, or cluster state. An absolute binary path,
read-only copied binary, checksum, or cleaned PATH protects only one replaceable input.

## Failure and replay behavior

A legitimate failed grade means the trusted observation completed and the approved
predicate was false. Deadline expiry, worker unavailability, API or credential
failure, malformed evidence, receipt-binding mismatch, invalid attestation, resource
exhaustion, cancellation, fencing, and unknown outcomes are infrastructure/security
failures and must not be recorded as a learner failure or pass.

The same idempotency key and request hash returns the same terminal receipt or reports
that the operation remains busy. It never starts a second verifier effect. Reusing a
key for a different allocation, generation, revision, artifact, deadline, or other
bound input is an idempotency conflict. A new user-requested verification uses a new
key. Controller restart must not speculatively re-execute an operation whose outcome
is unknown.

## Required acceptance evidence

The public verifier gate remains closed until automated conformance tests and live
provider tests prove all of the following:

1. Guest root replaces PATH tools, `/bin/kubectl`, the shell, kubeconfig,
   certificates, DNS, routes, and configured API endpoint without forging a pass.
2. Guest root replaces, reconfigures, or serves a fake learner-owned Kubernetes API
   without affecting the trusted API authority or producing an accepted receipt.
3. A correct repair passes and every current broken state fails through the trusted
   path. Semantic decoys fail for every problem predicate.
4. Cross-user, cross-allocation, cross-generation, expired, replayed, revoked, and
   wrong-audience credentials are denied. No verifier credential or private key is
   observable from the guest filesystem, environment, processes, terminal, or network.
5. Altered receipt fields, wrong problem or verifier digest, invalid authentication,
   stale controller fence, results after deadline, and late results from an older
   generation are rejected without changing the grade.
6. Concurrent same-key calls execute at most one verifier operation and replay one
   identical receipt. Same-key/different-request calls fail with an idempotency
   conflict. Runner or Control Plane restart and response loss do not create a second
   effect or accept two terminal results.
7. The worker cannot reach Proxmox/cloud APIs, Runner management, PostgreSQL, the
   Control Plane data plane, home LAN, metadata services, Internet destinations, or
   another session. It can reach only its exact trusted API and explicitly required
   infrastructure.
8. Timeout and cancellation terminate the worker, revoke its credential, and leave no
   worker, credential, network, or allocation resource orphaned.
9. Secret-like values, kubeconfig and certificate material, private endpoints, raw
   stdout/stderr, and provider errors injected into diagnostics never appear in
   attempt logs, lifecycle events, REST responses, or WebSocket frames. Public
   feedback is accepted only when its code and parameters match the allowlist.
10. The verifier image, verifier artifact, trusted API identity, and problem revision
    observed in the receipt match reproducible, reviewed, digest-pinned release
    artifacts.

Passing unit tests for receipt parsing or the Local Docker adapter is necessary but
not sufficient. Acceptance requires the adversarial tests above against the actual
Proxmox/KVM topology and network policy. Future cloud providers must pass the same
provider-neutral suite.

## Consequences

- Public Proxmox development may continue on lifecycle, provisioning, terminal,
  cleanup, and isolation seams, but public traffic and public grade claims remain
  disabled until this decision is implemented and evidenced.
- The Runner contract needs structured verification request/receipt and public
  feedback types rather than treating bounded stdout as a trusted result.
- The private infrastructure implementation needs a verifier worker, credential
  broker, trusted per-session API topology, network policy, artifact provenance, and
  private diagnostic storage.
- Local Docker remains useful for trusted development and functional testing, but
  cannot advertise or emulate the `trusted_external` capability.
