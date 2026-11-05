# Exact-ID historical reconciliation

Status: accepted.

## Context

[Account-bound storage](0002-account-bound-storage.md) preserves local numbers,
opaque identities and historical provenance. [Graph integration](0003-graph-provider-integration.md)
keeps authenticated subject evidence separate from historical ownership. A
reconciliation that blocks on matching headers between different IDs can prevent
inspection even when both exact originals remain available. An exact original
can also retain its identity after moving to another collection.

## Decision

Complete bounded primary-mailbox enumeration precedes independent inspection of
every original live immutable ID. Reuse the adapter's read-only inspection
batches, authenticated subject checks, immutable-ID preference and bounded read
retries. Duplicate identities, malformed or partial results, failed reads,
exhausted throttling and cancellation invalidate the entire evidence pass.
Required selected metadata must be present and non-null in successful inspection
responses; explicit empty strings, empty recipient arrays and false flags remain
valid. Optional sender/from and revision semantics are unchanged. A selected
null must not silently become a successful empty metadata value.

A found original must match its exact account-bound reference, locator, header
and sender. Enumeration and inspection must agree on current metadata, membership
and any recorded revision. Different opaque IDs remain separate even when their headers,
senders or content match. Correlation neither blocks exact verification nor
establishes ownership, continuity or authority to remap an original.

Unavailability requires both complete nonobservation of that exact original and
its own recognized Graph item-not-found response. A counterpart cannot supply
this evidence or turn a failed lookup into absence. At least one exact original
live anchor remains required when unavailable originals are proposed. Missing
old-source collection mappings remain blocked rather than guessed.

Reconciliation alone separates old mapped membership from current exact-ID
membership. Version-2 receipt items retain the original folder and memberships,
current state and an explicit change flag. The new proposal digest domain covers
those fields, all dispositions, account/provider, complete history digest and
folder mappings. The owner reviews every old-to-current change and confirms ALL
history using the exact account, history and reconciliation confirmations.
Execute regenerates evidence under exclusive lifecycle ownership; changed
evidence requires a fresh preview and review.

Strict binding and generic message/action comparators retain membership equality.
Existing challenge, cardinality, anchor, digest, verified backup, lifecycle and
transaction guards remain. Binding preserves all original columns, local numbers,
Graph IDs and history, while atomically adding attribution, current memberships
and a receipt. Unavailable originals retain their reserved identities without
invented current state. Later normal sync reuses the same ID's own number;
another ID retains or receives a separate number and no copied provenance.
Newly observed nonhistorical IDs are not published during attribution. Historical
pending plans remain readable and cannot acquire execution preconditions.

Receipt JSON evolves within the existing schema. Committed version-1 receipts
remain offline audit records and support same-account routing repair with their
exact stored confirmations. They are not rewritten or replayed as new binding
evidence. Version-1 pending proposals cannot authorize version-2 attribution.
No receipt, design, classification or capability grants mailbox action approval.

## Alternatives and consequences

Blanket correlation blocking prevents independent verification. Header/content
remapping merges distinct occurrences. Relaxing generic comparators weakens
action safety. Replacing history with a fresh mirror loses continuity. These
alternatives are rejected.

Batching reduces HTTP envelopes while retaining one lookup per original and
bounded retries. Enumeration is not an atomic snapshot or an archive/recoverable
store survey. Scoped unavailability proves neither permanent deletion nor
ownership. Full inventory, digest, backup and receipt costs remain. See the
[operational guide](../graph-reconciliation.md) for review and recovery limits.
