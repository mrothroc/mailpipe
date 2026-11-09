# Reconciliation approval and observation audit

Status: accepted. Supersedes only the raw-observation contribution to approval
identity in [ADR 0010](0010-exact-id-reconciliation.md). Its admission, identity,
history and action boundaries remain in force.

## Context

A complete fresh enumeration can have an already-permitted presentation difference
while every inspected state and historical attribution input remains identical.
Hashing full raw items makes approval depend on that variation, including through
derived flags or the presence of a retained enumeration pair. Removing validation
would hide invalid observations. Approval and audit need different identities.

## Decision

Every new proposal uses receipt version 8 and two SHA-256 digests over typed JSON
payloads. `reconciliation_digest` is the approval accepted by the existing
`--confirm-reconciliation` flag. `audit_digest` identifies the current observation
record and cannot authorize attribution or mail actions.

Approval uses domain `mailpipe-legacy-reconciliation-approval-v8`, exact account
and provider, complete historical source digest, coverage, folder mappings and
items sorted by original local number. Each approval item explicitly includes
its number, original opaque ID, original removed status, disposition/reason,
original folder/memberships, membership-change flag and complete inspected state.
That state includes exact metadata, reference/locator, revision and memberships.
Enumeration state, pair presence and all five enumeration-derived presentation
flags are excluded. Acquisition times/counts and later confirmation/backup outcome
fields are excluded. No names, addresses or inspected state are normalized.

Audit uses domain `mailpipe-legacy-reconciliation-audit-v8`, the computed approval,
account/source/coverage/folders, observation times/counts and complete raw items:
retained enumeration state, inspected state and every derived flag. Existing
optional absence stays absent; no missing enumeration state is reconstructed from
inspection. The collector retains pairs for admitted metadata differences; this
is not an all-record raw enumeration archive or wire-response export.

Both identities are computed only after unchanged complete-enumeration,
exact-inspection and independent store validation. Invalid enumeration still
fails with zero evidence, approval and audit, even when the invalid field is
excluded from approval. No new metadata relationship, mapping, alias, retry,
request or ownership proof is introduced. Typed payloads have fixed field names
and declaration order; standard JSON string-map key sorting is reused.

Fresh execution must match the reviewed account, history and stable approval.
Its audit can differ from preview when already-permitted enumeration presentation
changes. There is no prior audit input or `--confirm-audit` flag. CLI output
reports the fresh audit without claiming to detect a change from an unavailable
preview audit.

Within that one execution, the initial deeply owned evidence and receipt are
frozen before backup. Final transaction checks recompute the receipt's two
payload identities, independently re-review the same owned evidence against
original authoritative history and require both identities to match that
execution's snapshot. Approval must also match the owner's confirmation.
A permitted raw change inside the frozen snapshot is therefore stale even if
its approval projection stays identical. The canonical regenerated receipt,
actual raw observations, both digests and the verified backup reference/digest
in the receipt commit atomically. Backup creation and verification precede the
SQL transaction.

All source/backup digests, lifecycle locks, historical sender/header checks,
complete disjoint inventory/challenge, unavailable exact-404-plus-nonobservation
and live-anchor rules, and ALL-history confirmation remain. Original columns,
numbers, IDs, bodies, verdicts, filing, plans/actions and reservations are preserved.
Strict binding, ordinary sync/actions and Graph admission/transport are unchanged.
Inspection changes require a new approval or fail validation; display metadata
and an audit digest never establish historical ownership.

## Compatibility and consequences

Version-1 through version-7 pending confirmations cannot acquire version-8
semantics, even without presentation differences. Committed receipts remain
byte-immutable offline records; same-account routing repair uses their exact
stored account/history/confirmation. Old receipts have no invented audit digest
and are not rehashed, rewritten or replayed through binding. The existing JSON
receipt tables suffice; no schema migration or repair/import path is added.

The full private report still contains untrusted metadata. Primitive JSON/TSV
redaction, terminal escaping, complete private overflow and value-free failures
remain required. The audit is neither a signature nor a source attestation.
Serialization/hash work scales with retained evidence; no constant-memory claim
is made. See [reconciliation](../graph-reconciliation.md) for current review and
[recovery](../recovery.md) for preserved history and routing repair.
