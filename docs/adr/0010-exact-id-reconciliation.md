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
and original sender address. Enumeration and inspection must agree on current
metadata, membership and any recorded revision, with reviewed recovery-only
exceptions: sender and corresponding recipient address strings may differ under
Go's `strings.EqualFold` Unicode simple case folding. Recipient slice
representation, length, order and display names remain exact;
duplicate entries are retained. Sender display names may differ as reviewed
presentation metadata on that same exact ID. Changed name pairs must be valid
UTF-8 and retain both exact decoded strings, including empty or whitespace
spellings; no name normalization or identity inference is performed. All
remaining metadata stays exact. Address comparison adds no whitespace trimming,
full-fold expansion, Unicode normalization,
alias interpretation or global address rewriting. This is a reviewed string
relationship on the same immutable ID, not RFC mailbox equivalence. Different
opaque IDs remain separate even when their headers,
senders or content match. Correlation neither blocks exact verification nor
establishes ownership, continuity or authority to remap an original.

Unavailability requires both complete nonobservation of that exact original and
its own recognized Graph item-not-found response. A counterpart cannot supply
this evidence or turn a failed lookup into absence. At least one exact original
live anchor remains required when unavailable originals are proposed. Missing
old-source collection mappings remain blocked rather than guessed.

Reconciliation alone separates old mapped membership from current exact-ID
membership. Version-2 receipt items retain the original folder and memberships,
current state and an explicit change flag. Version-3 items additionally retain
`enumerated_state` for each accepted recipient casing difference, alongside the
exact inspection `state` and derived `recipient_casing_changed` flag. Version-4
items extend this to sender casing with a separate `sender_casing_changed` flag;
either flag requires both complete raw states. Version-5 items add
`sender_name_changed`, derived independently from the raw sender names. Any
address-casing or sender-name flag requires the paired observations. The store
independently validates each indexed pair and owns copies of the evidence. The
proposal digest covers both exact observations, membership, address-casing and
sender-name flags, all dispositions, account/provider, complete history digest
and folder mappings. Names and address casing are not normalized in stored
observations or the digest. The owner reviews
every old-to-current change, address-casing pair and sender-name pair, and
confirms ALL
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

Receipt JSON evolves within the existing schema. New proposals use version 5
and digest domain `mailpipe-legacy-reconciliation-v5`, even without metadata
differences. Pending version-1 through version-4 confirmations cannot authorize
new binding.
Committed version-1 through version-4 receipts
remain offline audit records and support same-account routing repair with their
exact stored confirmations. They are not rewritten or replayed as new binding
evidence.
No receipt, design, classification or capability grants mailbox action approval.

Unicode folding follows the checked Go compiler's tables. Keep the existing
[compiler policy](../development.md#build-gate) and version sources in
[go.mod](../../go.mod) and [CI](../../.github/workflows/ci.yml); no separate
Unicode canonicalizer or exact compiler pin is introduced. Table changes can
affect which pairs pass, but the digest always hashes the raw pair, not folded
keys. Changed raw names or address casing therefore require fresh review.
Sender presentation differences establish neither ownership nor continuity: the
independent exact reference, account, header, original sender address, membership
and revision checks still govern. Recipient names and all non-permitted future
fields remain strict.

Rejected metadata comparisons report only fixed field labels and positional
string relationships. Address relations select invalid encoding, exact,
untrimmed case-only, trim-only, trim-and-case, then different; recipient nilness
and cardinality differences take precedence. Recipient names remain exact.
Trimming is diagnostic only, never accepted evidence. Labels disclose no values,
add no reads and cannot authorize a proposal or action. Cause identity and zero
evidence on failure remain unchanged.

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
