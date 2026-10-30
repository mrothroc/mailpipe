# Account-bound mailbox storage

Status: accepted technical foundation.

## Context

A mirror's local message numbers, verdicts and reviewed action history must
survive provider adoption. A Graph identifier, an IMAP occurrence and a Gmail
message with several labels have different identity and membership semantics.
Neither an RFC Message-ID nor an EMAILID/content digest proves that two copies
are one stored message. Storage cannot authenticate a remote account by matching
mail content.

SQLite also owns its own database-file locks. On Unix, closing an auxiliary
file descriptor can cancel another connection's POSIX locks. Different aliases
for one database can select inconsistent journal names. These are documented
[SQLite corruption hazards](https://www.sqlite.org/howtocorrupt.html).

## Decision

One database is permanently bound to one account **and** provider. Other accounts
use separate files. There is no rebind operation. The account-aware API validates
the durable binding inside every transaction; the legacy Graph API accepts only
explicitly unbound stores. Binding requires exclusive lifecycle ownership, so an
old session cannot race a binding between reading a plan and contacting a server.
The current accountless application remains a legacy consumer until its own
integration adopts account-aware storage.

A persistent zero-byte sidecar, `<database>.mailpipe-lifecycle.lock`, owns lifecycle
coordination. Ordinary sessions share it, including concurrent handles in one
process; SQLite independently serializes their transactions. Schema maintenance,
creation, first binding and abandoned-pass cleanup require exclusive ownership.
The process manager counts local references, prevents upgrading another session's
shared lock, and evicts idle entries after verified SQL and descriptor closure.
It never deletes the sidecar to unlock it. A crash releases kernel locks.

Database and sidecar use the same canonical filename. Existing symlinks are
resolved before interpreting parent traversal; case and normalization aliases
are reconciled by filesystem identity without lowercasing distinct paths.
Hard-linked databases, sidecar symlinks/hardlinks, reserved journal/sidecar targets
and unsafe private directories are refused. Directory descriptors are pinned;
raw opens use no-follow, exclusive creation where appropriate, and close-on-exec.
A sidecar can never be a database, WAL or SHM inode. Only SQLite opens/closes an
**existing** main database. A newly created main descriptor closes before SQLite
admission; a missing/empty main with existing recovery siblings is refused.

SQL work drains before lifecycle release. Maintenance closes SQL before dropping
exclusive ownership, reacquires shared ownership and revalidates rather than
assuming an atomic lock downgrade. An uncertain SQL close retains ownership and
requires reconciliation/restart. A candidate sidecar descriptor whose identity
cannot be proved is neither locked nor closed: it is quarantined until process
exit, and further admissions fail. Closing that potentially misdirected descriptor
would recreate the lock hazard being prevented. Normal errors and cancellation
release verified resources; lifecycle contention, SQLite busy, I/O failure and
cancellation remain distinguishable. These guarantees require working local
filesystem locks and a stable private namespace. Older nonparticipating programs
and external writers must stop before maintenance or restoration.

Messages retain their exact integer primary keys and original Graph IDs. Graph
IDs become nullable with partial uniqueness; non-Graph records do not invent
Graph IDs. Provider identity and current opaque locator are separate columns.
Nonunique correlation indexes support lookup without deduplication. Collection
memberships are edges: adding/removing one label does not duplicate a message or
erase other edges, cached content, verdicts or history. IMAP tuples retain mailbox,
UIDVALIDITY, UID and a monotonic collection generation. An epoch reset retires
old identities/checkpoints while preserving their local numbers and audit data.
Detaching the old epoch's edges and updating the folder/removed projections are
one transaction, including when zero or multiple unrelated memberships remain.
New occurrences need new proven provider identities; numeric UID reuse cannot
resurrect an old plan.

Migration performs schema creation, historical plan-column repairs, explicit
primary-key copying, integrity/reference checks and version advancement in one
SQLite transaction. Parent rebuilding follows SQLite's documented order; foreign
keys are disabled only on the unexposed dedicated connection **before** beginning
the transaction, then restored and verified. Unknown schema extensions, future
versions and mixed attribution fail closed rather than being silently repaired.

Existing databases receive a verified consistent `VACUUM INTO` backup before
schema/binding changes. Backup bootstrap descriptors close before SQLite uses
them. A separate target lifecycle lock keeps the snapshot quiescent through
verification and fsync. Completed standalone snapshots are verified with a
strictly read-only immutable reader, never the normal mutating opener. The reader
closes before raw snapshot fsync. Logical digests include schema/version, types,
NULL distinctions and historical values, traversing SQL values in bounded
application chunks; SQLite owns its internal cache/statement allocations. A
locked source digest must still match the backup. Failed transactions roll back;
they do not trigger automatic file replacement. Offline restore preserves the
current recovery set, selects a fresh private destination under lifecycle locks,
and verifies it without creating WAL/SHM artifacts. Restoring an older backup
cannot preserve later writes without reconciliation.

First binding of historical Graph data is separate from empty-store binding.
Preparation inventories the exact database and live identities. A trusted adapter
must verify every live immutable ID, sender, header identity and mapped collection
against the explicitly authenticated account, and obtain confirmation that removed
records/history belong to it. Missing or inaccessible records require reconciliation.
Even an empty live inventory does not waive historical attribution. Storage checks
complete cardinality, challenge/account equality and the full digest again under
transaction lock. It never promotes old delta links into provider checkpoints or
invents execution preconditions for old plans. Those plans remain readable but
pending items need fresh inspection and review.

Provider sync stages complete pages privately. Only a validated terminal pass can
publish upserts, scope-limited removals and its checkpoint in one transaction.
Generation checks reject concurrent stale passes and changed epochs. The
session-owned pass capability also captures the latest local action audit ID at
begin. Commit checks the subsequent audit ID range for successful account actions
in the same transaction as publication; an intervening successful action requires
fresh evidence for the whole pass. This conservatively covers global metadata,
explicit removals and absence in other collection scopes. Failed/skipped attempts
and rolled-back batches do not invalidate a pass. Provider revisions remain
opaque and are never ordered. This adds no schema or reopen adoption path:
reopened sessions cannot assume ownership of another session's capability. A zero
terminal cursor clears the old token. Ordinary reopen preserves another session's
staging; only exclusive quiescent cleanup discards abandoned passes.

New plans snapshot durable account/provider identity, locator, metadata,
memberships, revision and epoch generations. Loading an executable request checks
that snapshot against the current mirror using the mailbox-domain comparator.
Action attempts retain immutable expected/returned evidence; successful state
changes and audit rows commit together. Unexplained identity changes require
reconciliation. Storage does not grant owner execution authority or close the
server inspection/execution race: adapters must inspect and recheck at execution,
and uncertain remote outcomes must be reconciled before retry.

## Alternatives and consequences

A single exclusive session would unnecessarily prevent ordinary concurrency.
Locking or raw-copying the live main file risks SQLite lock loss or an inconsistent
WAL backup. Implicit first-account attribution risks executing against the wrong
mailbox. One folder column or content-based deduplication loses memberships and
occurrences. Advancing checkpoints separately from durable effects can turn an
interrupted scan into false success. These alternatives are rejected.

The foundation adds explicit lifecycle and migration costs, needs private disk
for backups/staging, and deliberately refuses unsafe downgrade/rebinding. Frozen
historical fixtures, real pinned SQLite connections and independent processes
verify preservation, rollback/recovery, aliases, descriptor ordering, concurrency,
stale plans and checkpoint atomicity. They use invented data only. Provider and
application integration remain separate work.
