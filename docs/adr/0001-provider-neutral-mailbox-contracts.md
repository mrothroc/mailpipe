# Provider-neutral mailbox contracts

Status: accepted technical foundation.

## Context

The current application uses Microsoft Graph through `pkg/mailsync`,
`pkg/apply`, `pkg/rules` and CLI dependency construction. Its stored local
message numbers and reviewed action history must survive future provider work.
The current database is not yet account-bound. This decision introduces a
reusable domain boundary in `pkg/mailbox`; it does not migrate those consumers
or implement an adapter.

Folder membership, message identity and transport location are different
concepts. Gmail labels have a many-to-many relationship with messages
([label documentation](https://developers.google.com/workspace/gmail/api/guides/labels)).
IMAP identity depends on mailbox, UIDVALIDITY and UID; sequence numbers can be
reassigned ([RFC 9051, section 2.3.1](https://www.rfc-editor.org/rfc/rfc9051.html#section-2.3.1)).
A copied occurrence is not proved identical merely by sharing an RFC Message-ID,
body or sender. These constraints rule out a universal one-parent folder model
and content-derived message identity.

## Decision

`pkg/mailbox` imports only the standard library. It owns accounts, opaque
references, metadata, memberships, sync page shapes, capability boundaries and
fail-closed checks. It owns no transport, persistence, policy, credentials,
classification, scheduling or CLI presentation.

A `LocalRef` is a stable positive message number within an account. A
`MessageRef` carries account-bound provider identity separately from a current
opaque locator. A `Binding` associates them without allocating or deriving
numbers. Adapters choose the identity granularity they can prove: a stable
account-wide ID or a distinct scoped occurrence. Locators and provider revision
tokens contain no credentials and are never parsed or printed by domain helpers.
A change in scope or identity epoch must not silently reuse an old identity.
Proven identity changes after an action must be returned explicitly; later
storage integration preserves local handles/history only with that proof.

`State.Memberships` is an unordered set of account-bound collections. Zero,
one and multiple memberships are valid. Distinct occurrences remain distinct,
even with identical metadata. Collection display names need not be unique,
hierarchical or English names for action destinations.

Sync pages describe one collection. Upserts contain complete metadata;
removals detach only that collection's membership. Snapshot absence becomes
authoritative only after every page succeeds. Consumers stage a pass and save
its terminal checkpoint only after durable effects; failed/interrupted scans
cannot imply absence or commit a checkpoint. Cursors are opaque and bound to
account and collection. Invalidated cursors fail stale and require an explicit
fresh scan. Mode consistency across pages and transactional durability belong
to the future synchronizer/store integration.

Read access provides metadata, complete text, streaming MIME and current-state
inspection without marking mail read. No truncated-success result is allowed.
MIME callers discard partial output on error. Adapters should use a copy scratch
buffer of at most 32 KiB, separately document finite transport/parser/total
limits, check cancellation between chunks and use cancelable reads/deadlines.
The interface cannot interrupt an arbitrary blocking caller writer or bound
that writer's storage; callers needing those properties supply an appropriate
writer. MIME parsing and private spooling belong to the content layer.

Writes are separate optional interfaces for recoverable trash, documented
provider archive and native rules. Discovery and dispatch use the same Go
method-set assertions. Read-only wrappers must not embed writable methods and
attempt to disable them through flags. Capability availability is neither owner
approval nor a promise against later permission or network failure. Unsupported
semantics produce a typed actionable error, never a successful no-op, guessed
folder, permanent delete, alternate operation or automatic retry.

`ApplyReviewed` requires well-formed stored plan context, inspects the message
and checks exact identity/locator, RFC Message-ID, sender address, membership
set and any recorded revision. Sender comparison matches existing apply:
`strings.TrimSpace` and Unicode `strings.EqualFold`. Display names are excluded;
IDNA/punycode translation and Unicode normalization are not performed. Different
spellings that are not equal under that rule conservatively require fresh
inspection and review. Header equality never replaces opaque identity.

The caller must establish stored-plan ownership and owner execution authority.
A positive plan number is not approval. Preflight inspection is not atomic:
adapters must recheck immediately at execution, conditionally where supported.
Uncertain outcomes remain errors requiring reconciliation before retry.
Mail content, classification output and capability presence confer no authority.

Native rules model only exact-sender-to-recoverable-trash list/create/delete.
No display name, Graph sequence, query language or update method is required.
Unrepresentable rules remain visible by identity and cannot be round-tripped or
mutated. Callers establish managed ownership and review; adapters compare the
complete expected rule/version before deletion. Unsupported richer predicates,
effects, ordering and replacement fail rather than approximate. The existing
Graph rules implementation retains its fuller behavior, including junk rules;
this subset does not authorize replacing or weakening it. Native filtering and
scan-time policy remain distinct.

`ErrUnsupported`, `ErrStaleIdentity` and `ErrAccountMismatch` support `errors.Is`
and concrete error types support `errors.As`. Helpers return generic actionable
messages without echoing mail, locators or cursor tokens. Adapter/cancellation
errors are preserved through wrapping; adapters must also sanitize their errors.

## Alternatives and consequences

Reusing `apply.Mailbox` would carry folder/move semantics into every provider.
Using RFC Message-ID or content hashes would merge unrelated occurrences.
Exposing Graph requests, IMAP commands or generic wire payloads would invert the
adapter boundary. A fat interface with mutable capability flags and default
success methods would permit false support. These alternatives are rejected.

This additive package deliberately leaves current Graph consumers unchanged.
Later storage, Graph, IMAP and content work must prove migration safety,
checkpoint durability, bounded resources and actual advertised capabilities.
Contract tests verify the generic checks and illustrate adapters with invented
in-memory fixtures; they do not prove the behavior of an unbuilt live adapter.
