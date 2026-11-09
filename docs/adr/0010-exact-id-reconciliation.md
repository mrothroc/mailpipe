# Exact-ID historical reconciliation

Status: accepted. Raw-observation approval identity is superseded by
[ADR 0011](0011-reconciliation-approval-audit-digests.md); other constraints remain.

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
representation, length, indexed address relationships and duplicate multiplicity
remain strict. Sender and indexed recipient display names may differ as reviewed
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
every old-to-current change, address-casing pair and display-name pair, and
confirms ALL
history using the exact account, history and reconciliation confirmations.
Execute regenerates evidence under exclusive lifecycle ownership; changed
evidence requires a fresh preview and review.

Version-6 recovery may select the exact inspection sender when it still passes
the original historical sender/header checks for the same authenticated account
and immutable reference. Both decode seams must independently validate the raw
From/Sender choice and To-then-Cc mapping against the mapped state. Recovery
retains only per-reference validity stamps by default; raw source traces require
the explicit private diagnostic. False or unknown provenance cannot admit this
new exception. Existing exact, casing and sender-name behavior is preserved.
Changed noncase addresses must be nonempty valid UTF-8. This is directional
observation selection, not alias equivalence, normalization or current-sender
authority over original history. Only enumeration's sender address is excluded
on value copies for this reviewed comparison; recipient address order/shape, other
metadata, exact identity/header, membership and recorded revision remain strict.
Both complete raw states and the independently derived `sender_address_disagrees`
flag are digest-bound. That flag and case-only `sender_casing_changed` are mutually
exclusive; sender-name and recipient-casing flags remain independent.

Version-7 items add `recipient_name_changed`, derived from byte differences in
names at corresponding recipient positions. `recipient_casing_changed` derives
only from indexed address spellings; name-only changes never set it. Either
exception retains both complete raw states, independently validated by storage
and covered by the proposal digest. Full recipient value copies clear only the
approved Address/Name fields for comparison, so future fields remain strict.
Changed names must be valid UTF-8; unchanged names preserve the previous path.
No names are normalized or used as identity evidence. Inspection is current
metadata; enumeration remains review evidence and original history is unchanged.

Comparison follows the existing deterministic To-then-Cc sequence. Neither layer
sorts, matches or deduplicates recipients. Indistinguishable duplicate address
slots retain position-only comparison: there is no cross-slot identity inference
or claim to distinguish an unobservable swap. Both full arrays remain reviewable.

Strict binding and generic message/action comparators retain membership equality.
Existing challenge, cardinality, anchor, digest, verified backup, lifecycle and
transaction guards remain. Binding preserves all original columns, local numbers,
Graph IDs and history, while atomically adding attribution, current memberships
and a receipt. Unavailable originals retain their reserved identities without
invented current state. Later normal sync reuses the same ID's own number;
another ID retains or receives a separate number and no copied provenance.
Newly observed nonhistorical IDs are not published during attribution. Historical
pending plans remain readable and cannot acquire execution preconditions.

Receipt JSON evolves within the existing schema. New proposals use version 7
and digest domain `mailpipe-legacy-reconciliation-v7`, even without metadata
differences. Pending version-1 through version-6 confirmations cannot authorize
new binding.
Committed version-1 through version-6 receipts
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
and revision checks still govern. Recipient address relationships, positions,
shape, multiplicity and all non-permitted future fields remain strict.

Rejected metadata comparisons report only fixed field labels and positional
string relationships. Address relations select invalid encoding, exact,
untrimmed case-only, trim-only, trim-and-case, then different; recipient nilness
and cardinality differences take precedence. Name diagnostic relations compare
exact decoded strings and remain independent of admission.
Trimming is diagnostic only, never accepted evidence. Labels disclose no values,
add no reads and cannot authorize a proposal or action. Cause identity and zero
evidence on failure remain unchanged.

An explicit preview-only sender-address diagnostic can retain one rejected
same-reference enumeration/inspection pair in an owner-only, bounded private
file. Default collection retains no new raw error payload, and ordinary errors
remain value-free. This optional observation contains no proof or proposal;
it cannot grant admission or change receipt/digest semantics or attribution guards.
The private format independently checks the selected response's From/Sender choice and
To-then-Cc mapping, retaining only raw From/Sender addresses and fixed integrity
results in the explicit private artifact. Missing/null and present-empty From
are distinct. Unknown or ambiguous provenance fails rather than validating an
application mapping defect. The ordinary public inspection path has no observer;
the optional private recovery observer adds no transport, auth or requests.
An explicit original-number selector shares complete enumeration and ordered
inspection of all originals with normal reconciliation, but returns only a scalar
observation, never admitted evidence. It is independent of admission's first-error
loop. Format 3 distinguishes this requested, unvalidated observation from a
first-failure sender disagreement. Equal addresses can be reported; unrelated
metadata failures are not waived. Selected mode always exits runtime 1 with empty
stdout, even after saving. It never reviews a proposal or validates history.
It cannot accompany execution/confirmation or committed receipt use. Exclusive
rooted creation, privacy/identity checks and complete write/readback/closure
precede any saved hint. Uncertain output remains failed and non-authoritative.
See the [private diagnostic workflow](../graph-reconciliation.md#private-sender-address-diagnostics)
for safe inspection, storage and owner-managed cleanup of untrusted raw data.

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
