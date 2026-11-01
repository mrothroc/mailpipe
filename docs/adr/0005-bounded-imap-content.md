# Bounded IMAP content

Status: accepted technical design.

## Context and decision

Read-only synchronization supplies envelope, flags, size and exact occurrence
identity. Loading full MIME for every metadata sweep would repeatedly download
unchanged messages. `imapmail.ContentMetadataLoader` instead requests bounded
BODYSTRUCTURE through the synchronous scoped access lease. It derives attachment
evidence from complete disposition/parameter structure, refuses insufficient
structure, and leaves the optional display preview empty. An empty preview does
not establish an empty body. Repeated metadata scans do not fetch full source.

`imapmail.Client` supplies real `Account`, `Collections`, `Metadata`, `Inspect`,
`Text` and `WriteMIME` methods. This is a content/read subset, not a claim to
implement `mailbox.Provider`: the existing `SyncCollection` owns a whole admitted
pass, while `Provider.Sync` returns pages. Provider routing/composition must use
that actual distinction without a dummy method, nested complete pass or second
publication lifecycle. No mutable capabilities are added.

Source reads retain canonical account/mailbox/UIDVALIDITY/UID identity checks,
read-only EXAMINE, complete BODY.PEEK chunks, exact sizes/offsets/literals and
successful tagged completion. Source announcements reserve quota before the first
body chunk; actual streamed bytes and exact announced totals remain independently
checked. Quoted strings and literal bytes do not contribute to the wire grammar
nesting counter. The counter refuses excessive depth before codec recursion.

`document.Scope` owns private bounded spools for one operation, including all
concurrent messages and every raw, transfer-decoded, charset-converted and derived
representation. `OpenSource` publishes only complete identity-bound input;
`ParseReader` owns streaming MIME traversal and spool-backed part descriptors.
`Parse` shares the traversal as a finite compatibility materializer. Existing
`ReadSource` remains a whole-source compatibility collector with its 64 MiB guard;
production IMAP, hook and filing content paths use streaming APIs.

Plain-text selection prefers a complete plain alternative, otherwise readable
HTML. Mixed segments retain source order and trailing dates. Related content uses
its declared root, or first part; inline images remain available by Content-ID.
Attached nested messages/containers remain attachments. Ignored alternatives and
unknown body types still undergo complete bounded transfer validation. Unknown
charsets, invalid UTF-8/ASCII, malformed transfer encodings and ambiguous legacy
decoder replacement characters are protective failures, not repaired successes.

The shared HTML readable-text helper uses the maintained `x/net/html` tokenizer
with finite token buffers/counts and suppression depth. It emits text/entities,
image alternative text and structural separators, suppressing executable/embedded
content. It never resolves URLs, opens message-named files, runs scripts or creates
a browser. Plain text is never interpreted as HTML. Existing rendering sanitizes
parsed HTML, places its restrictive CSP first, and retains its isolated profile and
proxy. Streaming offline pages charge inline base64 expansion to the same quota.

## Finite limits

All values below are binary bytes. `DefaultLimits` supplies defaults. An all-zero
limits value selects defaults; otherwise every field must be positive and within
its ceiling. Invalid, overflowed or inconsistent values are refused. No unlimited
sentinel, private configuration change or new CLI flag is implied by this reusable
technical API. IMAP's existing `SourceBytes` and the content raw limit combine by
minimum.

| Resource | Default | Hard ceiling |
|---|---:|---:|
| Raw source/message | 64 MiB | 64 MiB |
| Cumulative decoded representations/message | 128 MiB | 256 MiB |
| Transfer-decoded part | 32 MiB | 64 MiB |
| Complete UTF-8/readable text | 8 MiB | 32 MiB |
| Derived offline page | 16 MiB | 32 MiB |
| Returned rendered PDF | 64 MiB | 64 MiB |
| Small consumer materialization allowance | 1 MiB | 4 MiB |
| Compatibility materialization | 64 MiB | 64 MiB |
| Logical memory reservations/scope | 64 MiB | 256 MiB |
| Aggregate spool bytes/scope | 256 MiB | 1 GiB |
| Header bytes/node | 64 KiB | 256 KiB |
| Total original header bytes/message | 1 MiB | 8 MiB |
| Header fields/node | 256 | 1,024 |
| MIME nesting (root depth zero) | 20 | 32 |
| MIME nodes, including root/ignored/nested parts | 256 | 1,024 |
| Boundary bytes | 70 | 70 |
| HTML token buffer | 64 KiB | 256 KiB |
| HTML tokens | 1,000,000 | 4,000,000 |
| HTML depth/suppression stack | 128 | 256 |
| Concurrent content pipelines/scope | 4 | 16 |
| Owned files/scope | 1,024 | 4,096 |
| Outstanding reservations/scope | 1,024 | 4,096 |
| Source/parser/body/page timeout | 2 minutes | 10 minutes |

Copy/decoder chunks are at most 32 KiB. Existing transport limits remain separate:
64 KiB codec literals, 128 KiB responses, 8 MiB command responses, grammar depth
128 and 30-second command deadlines under the existing ten-minute operation cap.
Earlier caller deadlines win. No transport/parser/caller buffer is described as
an exact RSS bound. Large decoded attachments remain file readers, never arbitrary
payload-sized byte slices.

The memory budget reserves parser/header/descriptor estimates and explicit
materializations before allocation. Estimates include depth-dependent decoder and
lookahead buffers, bounded header/string/map overhead, descriptors and token
capacity. The small allowance is a materialization threshold, not permission to
exceed the total reservation budget. Compatibility DTOs, caller-returned strings,
SQLite's existing full-content readers, allocator/stack/transport internals and
the browser are separate from streaming parser retained-capacity claims. The
renderer returns a byte slice before its result-size check; this does not enforce
a renderer allocation or browser RSS ceiling.

Original multipart header bytes, including folded whitespace, are counted before
header parsing. A gate also bounds header-reader lookahead and preamble work;
stdlib's larger internal header limit cannot cause an arbitrary allocation. Root
headers use a bounded line scanner. Transfer output and UTF-8 conversion output
are both charged as cumulative decoded work; this is not a count of unique
information bytes. Derived text/pages have independent limits and disk charges.

## Quota, ownership and cleanup

A CLI content operation shares one scope across its workers, body callbacks and
hooks. A client has a fallback scope for its lifetime when a caller supplies none.
No per-message quota reset evades shared accounting. Different processes/scopes
have independent budgets; this is not a machine-global disk or free-space promise.
All simultaneous raw/decoded/derived files count, including encoded and expanded
copies. Logical bytes do not measure filesystem allocation blocks/metadata.

A mutex protects reservations, used bytes and the bounded registry; it is not held
across file/network/caller I/O. Announcements reserve before transfer. Every actual
write obtains budget before crossing it; successful bytes consume reservations
without double counting. Short/uncertain writes retain conservative charges.
Memory reservations similarly precede materialization. Exactly-at-limit content
can succeed; the next byte is refused without publishing a partial source.

The root is `os.TempDir()`, whose environment-selected location is not assumed
private. Its canonical parent is opened and pinned. The parent must be owned by
the effective user or root; other-user-writable parents require sticky semantics.
An unpredictable exclusive operation child is verified by descriptor as owned and
0700. Exclusive 0600 regular files require effective UID, single-link and identity
proofs. Reopens reject symlinks explicitly with Lstat and verify the opened inode,
mode and size. `os.Root` pins containment; an O_NOFOLLOW flag alone is insufficient
because rooted path resolution may resolve a contained symlink first. Message
filenames/IDs never choose spool paths. Same-UID hostile modification is outside
the stable-private-namespace assumption; unsupported ownership proofs fail closed.

Completed representations are removed promptly, including intermediate charset
spools and unselected bodies. Mixed parts compact as they are consumed. Each
message closes its remaining resources before accepting text/classifier/filing
success; hooks retain complete source through child exit, then clean it before
accepting a verdict. Admission and message boundaries perform bounded checkpoints
(at most 32 pending owned entries); prompt per-message cleanup is stricter than
the sixteen-message checkpoint cadence. Active readers/nonowned entries are not
swept. Failed close/unlink keeps its charge and poisons subsequent admissions.
Verified later removal can release bytes but does not erase the surfaced failure.

Cleanup uses `context.WithTimeout(context.WithoutCancel(operationContext),
5*time.Second)`: operation values survive, parent cancellation/deadline does not.
One message cleanup shares that deadline across its files. Terminal scope cleanup
joins admitted cooperative work and handles only its finite owned registry. A
blocked filesystem syscall/arbitrary writer cannot be forcibly terminated by a
context; no abandoned timeout goroutine pretends otherwise. Failures remain
visible and cannot falsely release quota or report success. No automatic janitor
adopts prior directories based on names/PIDs. Crash leftovers require explicit
quiescent operator inspection; this protocol claims no cross-crash global quota.

## Consumers and failure effects

Only complete successfully cleaned text reaches the existing account-bound body
cache. Cached empty bodies and offline lazy authentication semantics remain.
Hooks keep the same JSON `source_path`/cost contract, exact sender scope, minimal
environment, direct argv, serial budget and prohibition on action/final output.
Production callbacks stream into the owned source file instead of duplicating a
whole-source byte slice.

Incomplete/over-limit content makes no model call and cannot produce a fingerprint
or filing match. The stage runner creates a valid `review`/`keep` safety verdict.
On cleanup failure the sink persists that protective verdict as successful policy
evidence while separately surfacing a fatal run error; an error-only row must not
leave an older destructive verdict active. Cancellation stops work without a
partial decision. A post-child hook cleanup failure discards the child's result;
already completed calls cannot be retroactively undone, and no subsequent call is
admitted. Complete State still includes headers and all readable text before the
existing `max_input_bytes` check (default 1 MiB). No shortened retry, lower
confidence or model-limit bypass is permitted.

Filing streams PDF attachments through rooted exclusive 0600 temporary outputs,
fsync, no-replacement linking and bounded readback/comparison. Existing PDF date
normalization uses a bounded 1,024-byte token scanner; longer comparison tokens
fail rather than overwrite. Owner-approved destination outputs are distinct from
content spools and retain their own finite output limits. No saved verdict is
recorded until required content cleanup and output verification succeed.

## Alternatives and evidence

Rejected alternatives include full-source metadata sweeps, guessed attachment
flags, new store projection/SQL escape, regex HTML stripping, browser text
extraction, unbounded attachment buffers, per-message-only disk limits, PID-based
sweeping, and an invented page-returning sync stub.

Generated tests cover exact and plus-one quotas, concurrent reservations,
expansion, original folded headers, depth/part bounds, cancellation, macOS
fstat/UID/mode/sticky parents, symlink substitution, cleanup failures and retained
charges. Actual protocol/SQLite tests prove read flags, occurrence identity and
metadata download behavior. Streaming byte digests and allocation observers have
independent full-buffer positive controls; they do not claim zero allocation or
RSS invariance. Mock-model and CLI tests prove zero partial-content calls and
persisted protection. Existing Graph, hook, template, offline rendering, rooted
filing and injection tests remain required.

The already pinned `golang.org/x/net` HTML/charset implementation uses the BSD
three-clause license and declares Go 1.26.0; its existing `x/text` dependency
provides transforms. No new parser dependency or toolchain minimum is required.
Normal module/version/vulnerability checks remain authoritative before commit.

References: [IMAP fetch semantics](https://www.rfc-editor.org/rfc/rfc9051.html#section-6.4.5),
[HTML tokenizer](https://pkg.go.dev/golang.org/x/net/html),
[charset readers](https://pkg.go.dev/golang.org/x/net/html/charset),
[module requirements](https://raw.githubusercontent.com/golang/net/v0.62.0/go.mod),
[license](https://raw.githubusercontent.com/golang/net/v0.62.0/LICENSE).
