# Reference-scoped historical reconciliation

Status: accepted. Supersedes primary-mailbox enumeration and paired-observation
admission in [ADR 0010](0010-exact-id-reconciliation.md) for new attribution.
Retains the approval/audit separation of [ADR 0011](0011-reconciliation-approval-audit-digests.md).

## Context

Historical attribution needs correctly associated observations of every original
immutable reference. Unrelated mailbox discovery adds list consistency and coverage
requirements without establishing historical ownership. A list or multi-reference
batch is not an atomic mailbox snapshot. Folder delta coverage is a separate
publication protocol; normal sync remains its owner.

## Decision

The supported reference reader inspects the complete historical live inventory
under one pinned authenticated Graph account. It uses the existing ordered GET
batch adapter, immutable-ID preference and bounded transport. It does not list
`/me/messages`, import delta semantics or fall back to legacy enumeration.
Opaque identifiers remain exact and case-sensitive. All reads are acquired before
historical admission; an original sender/header conflict does not skip later reads.
Any fatal read, cancellation or malformed response returns zero evidence.

Each found outcome needs a correctly associated raw 200 object, complete selected
metadata, unambiguous selected keys, no removal member, and verified source-to-state
mapping. Empty subject/preview, empty recipient arrays and optional From/Sender or
changeKey retain the adapter's existing semantics. Optional From is preferred over
Sender; recipients remain To then Cc. No new normalization or address equivalence
is introduced. Found states must pass original sender/header and exact reference
checks. Old/current memberships and current revision remain explicit review inputs.

Unavailable requires that reference's own recognized item-not-found result: valid
UTF-8 JSON object, canonical unambiguous error/code keys, HTTP 404 and matching typed
and raw `ErrorItemNotFound` code under the expected ordered request identity. Benign
unrelated annotations are ignored. Unknown, ambiguous or malformed errors never
prove unavailability. This means inaccessible through that exact reference at
observation time, not deletion, archive absence, ownership or alias equivalence.
At least one live original must verify when unavailable rows are proposed. Already
removed historical rows have no fabricated remote outcome or state.

Store independently validates the trusted reader's account, challenge, method,
coverage, complete disjoint counts, exact outcome references, source validation
stamps, original history and full states. It does not independently reread Graph
and accepts no editable report as evidence. Every original local number, opaque
identity reservation, legacy column, body, verdict, filing, plan and action survives.
Unknown source folders still block. Strict binding and ordinary actions/sync stay
unchanged; explicit ALL-history confirmation remains required.

New proposals use version 9, method `graph-exact-original-inspections-v1` and coverage
`graph-original-references-v1`. Typed approval domain
`mailpipe-legacy-reconciliation-approval-v9` binds method, account, complete source,
coverage, folder mappings and sorted dispositions, exact read references/outcomes,
old/current memberships and complete inspected states. Audit domain
`mailpipe-legacy-reconciliation-audit-v9` binds approval plus actual acquisition
times, requested/completed counts, logical batches, encoded state bytes, full items
and source validation stamps. No enumeration states or pair-derived flags are
invented. Fresh execution audit may differ; both digests must match THIS execution's
owned snapshot at final re-review. Backup creation and verification precede SQL;
the verified backup reference/digest and receipt commit atomically with attribution.
Committed versions 1–8 remain byte-immutable, available offline and eligible only
for exact stored-confirmation routing repair. Pending versions 1–8 cannot
authorize version 9.

The reader caps originals at 1,000,000 and aggregate encoded complete states at
256 MiB, without truncation. A cooperative 30-minute remote-read context respects an
earlier parent deadline; the parent remains available for backup and final SQL.
These are not whole-process memory or whole-command hard limits. History, copies,
receipts and backups still scale with inventory. Batch count is logical acquisition,
not retry telemetry. With L originals, F mapped historical folders and
B=ceil(L/20), the existing six-try nested batch/envelope policy permits at most
36B batch send attempts, 36L embedded GET appearances and 6F folder-read sends.
Redirect hops and token-provider work are separate. Existing 16 MiB response
limits and cancellation-aware waits remain. Existing finite transport/batch
retries remain; no global retry-until-quiet or automatic repeat of missing
references is added.

## Diagnostic compatibility

Explicit first-failure and selected-original private diagnostics retain the legacy
paired protocol and format 3. They always terminate without successor proof, store
review or proposal, even when the legacy collection succeeds. Existing secure,
bounded exclusive output and value-free ordinary errors remain. This diagnostic
capability cannot manufacture new coverage or attribution authority.

## Verification

Synthetic real-HTTP and ordinary CLI tests assert zero unrelated list requests,
all exact originals, strict response association and 404 envelopes, full-state
mapping, limits/cancellation and zero proof on failure. Store tests independently
exercise outcomes, histories, digests, ownership, final transaction mutation,
rollback, frozen old receipts and pending replay. Shared batch tests reject
unsolicited current-round responses without replaying uncertain mutations.
