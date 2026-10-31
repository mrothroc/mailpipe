# Graph provider integration

Status: accepted.

## Context

The CLI originally constructed Graph paths and decoded Graph JSON for sync,
content and reviewed actions. Storage now distinguishes unbound historical data
from a permanently attributed account. Migrating those consumers must preserve
local message numbers, historical records, command output, offline use and the
fuller managed junk/block rule behavior.

## Decision

`pkg/graph` owns authenticated HTTP, origin validation, bounded response reads,
immutable-ID preferences and retry mechanics. `pkg/graphmail` maps Graph wire
records, collections, delta links, complete content and reviewed trash into
`mailbox.Provider` and its optional capabilities. The CLI constructs adapters in
its dependency wiring; content and action callers consume domain references.
Graph batch size, response ordering and message/rule wire shapes stay in explicit
Graph adapters. The legacy-unbound apply adapter remains a compatibility API;
bound CLI actions load account-scoped reviewed requests instead.

A Graph account key encodes the resource tenant and user object identifier from
the successful MSAL authentication result. Preferred username remains display
information. Every token acquisition used by the adapter checks the same subject,
including refreshes between inspection and mutation. Missing or inconsistent
subject evidence fails closed. Configuration without a provider still selects
Graph. Private routing selects storage; it does not authenticate a remote mailbox
or override a permanent database binding.

Historical attribution is explicit. A verification preview inventories the exact
unbound database and checks every live immutable ID, sender, header identity and
collection against the authenticated mailbox. Binding requires the reviewed
inventory digest and confirmation that removed records and history belong to
that account. Storage performs its own challenge, cardinality, digest, backup and
transaction checks. Prebinding plans remain readable but cannot execute; a new
plan must be inspected and reviewed. No old delta link becomes a bound checkpoint.
A genuinely empty database can be attributed during its first authenticated sync.
Routing-save failure after binding is reported as partial local completion; retry
verifies the same binding rather than reassigning or replacing the database.

The command store facade translates legacy output/filter names into account-local
references and canonical memberships. Bodies remain lazy and cached, including
empty bodies. Local listing, digest, plans, verdicts, cached content, template and
filing checks need no sign-in. Graph IDs remain output compatibility data, not a
substitute for durable account-bound identity. Sent-recipient protection is derived
from account-scoped Sent Items metadata.

Atomic multi-message verdict recording belongs to storage. `BoundVerdict` pairs a
local reference and verdict; `AccountStore.AddVerdictsBatch` validates all account,
identity and nonzero-N constraints inside one existing transaction, then records
the entire ordered batch. Duplicate entries remain distinct. Any failed JSON/SQL
operation or cancellation rolls the transaction back. Empty batches still validate
the session. Existing same-message methods and valid detached-identity semantics
are unchanged. Imports and filing consumers call the additive batch API once per
original operation; no raw transaction handle is exposed.

`pkg/mailsync.RunPass` owns one reusable begin/stage/commit/abort lifecycle over
existing account-store primitives. Its `PassSpec`, `Scan` and `EmitPage` contracts
require only sync evidence, not text/MIME/inspection capabilities. Storage remains
the sole authority for cursor chains, modes, supplements, epoch/generation races,
action watermarks and atomic publication. A scanner owns its protocol traversal,
retry classification, completeness proof and resources. It closes and joins its
session child before returning, while retaining the operation context for commit.
Only terminal evidence plus a successful scanner return permits publication;
ignored staging errors and late scanner failures remain fatal.

On failure, cleanup aborts only the owned pass on the same handle using an
independent context with a five-second timeout. Original, driver and cleanup
errors remain joined; `ErrCleanup` prohibits another retry before reconciliation.
There is no automatic global cleanup, epoch retirement, public-call serialization
or new lifecycle lock. Protocol-specific traversal stays separate: Graph follows
opaque paged delta links; an IMAP scanner must prove its own UID/metadata inventory
and supply matching occurrence evidence. Both reuse the same lifecycle and storage
invariants instead of implementing competing publication engines.

Graph's partial delta records are hydrated before staging. A record that vanished
or moved out during hydration detaches only the requested collection. A full scan
uses terminal absence reconciliation. Checkpoints advance only with committed
pages, and a successful intervening local action invalidates stale publication.
Immutable IDs preserve local numbers through ordinary folder moves.

Reviewed trash checks exact account, opaque identity/locator, RFC Message-ID,
trimmed case-insensitive sender, memberships and recorded revision. The adapter
reinspects immediately before each move batch and proves complete returned state.
This is not a claimed atomic compare-and-move transaction: Graph does not document
such a condition for this operation. Read retries remain bounded and honor
Retry-After. Uncertain mutation transport/5xx/incomplete responses are not blindly
replayed. Explicit rejected throttled entries may retry only after fresh inspection;
successful entries never repeat. Audit and returned local state commit together.

Managed junk/block rules retain their richer Graph service. The narrower generic
native-rule subset does not replace junk predicates, managed ownership, ordering,
exact shape checks, replacements, retired rules, mailbox-wide kept-mail checks or
post-write readback. Complete original rule evidence remains private to the
adapter and is compared again before updating/removing a rule. Owner rules are
untouched. A read-only wrapper exposes no writable method set.

`document.MIMEReader` contains only `Account` and `WriteMIME`.
`document.ReadSource` validates an account-bound reference, fetches once, and
returns complete bounded source or nil bytes on error. The default source ceiling
is 67,108,864 bytes (64 MiB). Transport copy scratch is at most 32 KiB; the document
collector buffers the **whole source up to 64 MiB before parsing**. These are
different limits. Buffer growth and MIME parsing allocate additional memory;
this is not a constant-memory parser or an aggregate process-memory guarantee.
Graph metadata responses have a separate 16 MiB ceiling and text responses a
64 MiB ceiling. Multipart depth, decoded parts, attachments and spool quotas are
separate parser/content concerns. The bridge reuses the existing parser/hook
callback, with no second ad-hoc fetch path. Arbitrary blocking writers cannot be
force-canceled; transports and collaborators must cooperate with context.

A content limit uses the detail
`<operation>: message exceeds 67108864-byte content limit; incomplete content was discarded`,
with a fixed application operation label. CLI errors keep the normal command
wrapper, runtime exit and appropriate inspection/help hint. Partial content is
never cached, sent to a hook or recorded as filed. Existing private hook cleanup,
offline HTML/PDF isolation, rooted filing and no-overwrite behavior remain intact.

## Alternatives and consequences

A full Provider requirement for the synchronization core would force a sync-only
adapter to fabricate unrelated content capabilities. Duplicating pass publication
would split cleanup and correctness ownership. Per-message verdict commits would
weaken existing atomic imports. Binding by display address, copying old execution
preconditions, coercing rich rules into the native subset and replaying uncertain
moves are rejected.

The integration introduces explicit migration/reconciliation costs and finite
content errors. Tests use invented messages, local HTTP transports and temporary
SQLite stores; they cover compatibility, wrong accounts, incomplete content,
transaction rollback, concurrent pass/action refusal, cleanup, method-set
capabilities, retries/origin restrictions and offline filing without live resources.

References: [immutable Graph IDs](https://learn.microsoft.com/en-us/graph/outlook-immutable-id),
[identity claims](https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference),
and [Graph message move](https://learn.microsoft.com/en-us/graph/api/message-move?view=graph-rest-1.0).
