# Read-only IMAP discovery and synchronization

Status: accepted.

## Context

IMAP occurrences belong to an account, mailbox, UIDVALIDITY and UID. Sequence
numbers change on expunge; Message-ID, identical bytes and EMAILID cannot prove
occurrence identity. Yahoo's default limited view can return successful commands
and counts while hiding older mail. A complete local mirror needs explicit
full-view evidence, complete inventory verification and atomic publication.

## Decision

`pkg/imapmail` owns verified TLS, authentication, post-authentication CAPABILITY,
folder discovery, read-only protocol traversal and complete source reads. It uses
the MIT-licensed `github.com/emersion/go-imap` v1.2.1 root wire codec and UTF-7
utilities, whose module declares Go 1.13. The repository's newer supported Go
minimum remains unchanged. The high-level mutable client is never imported:
its mutation methods, implicit move fallback and expunging close are outside this
release. Reviewed transitive overrides select go-message v0.18.2 and go-sasl
v0.0.0-20241020182733-b788ff22d5a6; current module checks and vulnerability gates
remain mandatory, including modules outside the imported call graph.

The private command allowlist is CAPABILITY, LOGIN, LIST, ENABLE, EXAMINE and
UID FETCH. There is no raw execution API, mutation method, insecure TLS option,
STARTTLS downgrade or COPY/STORE/EXPUNGE fallback. Socket close does not issue
IMAP CLOSE. Production defaults use `imap.mail.yahoo.com:993`, TLS 1.2 or later,
normal certificate trust and hostname verification. Tests explicitly supply their
own generated CA and loopback endpoint; production does not discover fixture
trust. Credentials are supplied by a trusted callback in memory, with exact
configured account and login equality on reconnect. Configuration identifies the
account; an app-password login does not establish an immutable provider subject.

Each authenticated session issues fresh CAPABILITY. Generic IMAP works without
optional extensions. The Yahoo profile requires ENABLE, UIDONLY and the service's
CONDSTORE prerequisite, then requires acknowledged ENABLED UIDONLY before scanning.
It does not activate CONDSTORE, QRESYNC or OBJECTID optimizations. Advertised
mutation extensions do not create writable Go capabilities. Missing full-view
support fails rather than silently mirroring a capped subset.

Folder identities encode exact canonical paths. Only standard INBOX is
case-insensitive; localized Sent/Trash/Junk names are never guessed. Required
roles must have unique special-use evidence or explicit mappings to existing
selectable mailboxes. Missing, unknown, conflicting and ambiguous mappings fail;
no folders are created. Without proven object continuity, rename is a different
collection and disappearance requires reconciliation.

EXAMINE must supply read-only selection, nonzero UIDVALIDITY, UIDNEXT and count.
The scanner visits every finite numeric UID window through UIDNEXT-1, including
empty windows. Window width defaults to 250, reduced by positive advertised
MESSAGELIMIT and capped at 1000. Both ordinary numbered FETCH and RFC 9586
numbered UIDFETCH responses require explicit UID evidence; UIDFETCH prefixes
must match, and acknowledged UIDONLY also requires matching legacy FETCH prefixes.
Sequence prefixes never establish occurrence identity. A UID absent from the
prior complete inventory cannot newly reappear below its committed UIDNEXT in
the same epoch. Such reuse fails stale rather than reattaching an old local
number; explicit epoch reconciliation is required for legitimate epoch resets.
A MESSAGELIMIT response,
including tagged OK, is partial evidence. A validated smaller limit can trigger
only a fresh, bounded attempt after successful staging cleanup.

Complete metadata comes from a required `MetadataLoader`. The transport supplies
copied envelope/flags/date/size evidence and a scoped `ReadAccess` for exactly one
occurrence. No empty-success content provider is supplied. The later content
layer owns real MIME parsing, text, attachments, private spool implementation and
full Provider composition. The loader runs synchronously after the current FETCH
command's tagged completion. Scoped reads delegate the already-held operation
lease rather than reacquiring a non-reentrant lock. Access expires on Load return;
recursive/concurrent misuse and ignored source failures invalidate the pass.

BODY.PEEK source chunks verify UID, size, offset, literal length, tagged completion
and exact total. Partial output is discarded on error. Complete source defaults
to 67,108,864 bytes, with at most 32 KiB copy chunks, independently of the codec's
64 KiB literal, 128 KiB response and 8 MiB command-response limits. Writer storage,
MIME decode/depth/part limits and private spool quotas are separate concerns.
A writer or loader blocked in arbitrary caller code must cooperate with context;
no abandoned timeout goroutine pretends to terminate it. Client Close cancels
transport without acquiring the operation lease. Session cancellation callbacks
are stopped or joined; the session child closes before the scan returns while
the operation parent remains available for publication and bounded abort.

`mailsync.RunPass` exclusively owns Begin/Stage/Commit/Abort. The protocol callback
owns traversal, complete records/structured tuple supplements, cursor evidence,
resources and verification. Prior scoped inventory is read only after admission
captures the action watermark. Pages are emitted incrementally, never retained
for asynchronous use. Two complete UID/flags/size sweeps and a final EXAMINE must
agree on UIDs, high water, epoch and count. Dirty events, missing required data,
interrupted commands and late cleanup failures prevent terminal publication.
External moves detach the source occurrence and create a distinct target
occurrence. Snapshot absence and scoped removals retain local numbers/history.
This is a validated observation, not a remote transaction: later changes require
the next scan. Persistent churn or a budget limit returns failure.

Conditional epoch reconciliation belongs to storage. `ReadIMAPEpochState` captures
binding generation, collection/monotonic epoch and checkpoint-row presence,
generation, epoch and token in one transaction. `ReconcileIMAPEpoch` checks the
exact expected state and makes its first write a conditional collection update.
Only a successful guard permits retirement and checkpoint clearing in the same
transaction. Stale snapshots fail SQLite's write upgrade or the guard; no newer
publication is retired. The legacy unconditional invalidation API stays compatible
and shares private retirement/projection work. Ordinary sessions coexist; no
exclusive runtime lifecycle lock, schema change or raw transaction API is added.

## Inventory resource contract

`AccountStore.ReadIMAPInventory` returns only LocalRef, MessageRef and the structured
IMAP tuple. It does not return partial complete-message DTOs or load cached Body,
metadata JSON, HTML or attachments and discard them. Legacy full-content readers
keep their original behavior.

Hard bounds are 100,000 rows, 8,192 **bytes** per identity field and 67,108,864
bytes of logical retained-data charge. Each row is charged:

```
256 + 2*len(accountID) + len(collectionID) + len(mailboxID)
    + len(messageID) + len(locator)
```

One existing read transaction first checks scalar types, byte lengths and tuple
matches with a cap-plus-one query. Only after the entire preflight succeeds does
it project N, occurrence ID, locator and UID in the same snapshot. Oversized
identity strings are rejected before conversion into Go string values. Verified
common scope/epoch fields are reconstructed. Every error returns nil inventory,
never a truncated prefix or false empty success. Rows and transaction close
before any network, loader or page emission. Empty valid collections succeed.
The scanner retains bounded identity/UID indexes and emits at most 100 upserts or
removal references per page. Remote flag/size inventory also has a finite logical
byte budget; metadata/envelopes are retained only for a bounded UID window.

The charge is **not** an exact Go heap, allocator or process RSS ceiling. Slice
and map overhead is finite O(rows); SQLite can read database/overflow pages and
allocate its internal caches. Real pinned-driver tests inspect actual Columns and
Next-delivered values, with a positive full-content read proving payload detection.
Supplementary allocation measurements avoid brittle exact counts/RSS thresholds.
They do not claim zero engine I/O or engine allocation. Boundary, cancellation and
concurrent-publication tests preserve checkpoints/history; generation/epoch and
action-watermark guards remain the authority after the read snapshot ends.

## Retry and uncertainty

At most four attempts share a ten-minute operation deadline and finite window
budget. Direct transient disconnects, recognized bare UNAVAILABLE or THROTTLED
failures and verified consistency churn may retry after successful owned abort.
Backoff bases are 1, 2, 4 and capped 8 seconds with equal jitter in [base/2,base].
A trusted typed credential hint is a lower bound, capped at 30 seconds and never
allowed past the total deadline. No numeric server retry-hint syntax is assumed.
LIMIT, malformed codes, invalid credentials, TLS verification, unsupported
capabilities and resource/protocol failures are permanent. MESSAGELIMIT is a
window-size condition, not seconds of throttling. Successful commands carrying
THROTTLED do not cause replay.

A private per-attempt marker plus entered/returned callback evidence proves retry
provenance. Merely finding EOF/net.Error in an error chain is insufficient.
ErrCleanup vetoes retry first. Begin/admission, Stage, Commit, postcommit reporting,
local inventory, opaque loader/access/spool and resource-cleanup errors never
acquire protocol retry authority. Unknown commit acknowledgment is uncertainty,
not proof of rollback. Reconnection authenticates the same account/login,
rediscovers capabilities and revalidates mailbox/UIDVALIDITY before a fresh pass.
A mid-pass BYE closes the protocol child, returns the marker, and must finish the
core's owned abort before any backoff. No continuation resumes an incomplete pass.

## Alternatives and validation

A mutable library client, guessed role names, sequence identity, content/EMAILID
deduplication, capped-view success, a second publication engine, unconditional
observed resets and loading bodies before dropping them are rejected. Scripted
verified-TLS servers, invented example-domain data, real SQLite two-handle races,
driver projection controls, fake clocks and command audits test these boundaries.
No live mailbox, model endpoint, private configuration or real credential is needed.

References: [RFC 9051](https://www.rfc-editor.org/rfc/rfc9051.html),
[RFC 6154](https://www.rfc-editor.org/rfc/rfc6154.html),
[RFC 9586](https://www.rfc-editor.org/rfc/rfc9586.html),
[RFC 9738](https://www.rfc-editor.org/rfc/rfc9738.html),
[RFC 5530](https://www.rfc-editor.org/rfc/rfc5530.html),
[Yahoo IMAP defaults](https://help.yahoo.com/kb/SLN4075.html),
[Yahoo developer protocol documentation](https://senders.yahooinc.com/developer/documentation/),
[codec module](https://github.com/emersion/go-imap/tree/v1.2.1)
and [MIT license](https://github.com/emersion/go-imap/blob/v1.2.1/LICENSE).
