# Reviewed historical Graph reconciliation

Authentication, configuration routing and historical attribution are separate.
A stale cached reference can prevent strict `account bind` while an unattributed
mirror prevents sync. `account reconcile` provides a reviewed local recovery for
that case. It preserves the original database and message numbers; an isolated
survey database is not recovery evidence or a replacement for its history.

## Preview and approval

Stop older nonparticipating programs and close other database sessions. Preserve
the mirror and existing migration backups. Select the intended private
configuration and database consistently for every command. Start with the built
binary's `account reconcile --help`.

```sh
mailpipe account reconcile --json
```

Opening an old schema can perform a backed-up migration. The preview itself does
not bind history, save routing, publish sync observations or change server mail.
It inventories the complete database and inspects every original live immutable
reference under the authenticated account. It does not enumerate unrelated
mailbox messages or fetch bodies.

Review the immutable account, complete history digest, reconciliation approval
digest, separate observation audit digest,
coverage and observation times, counts, and **every message disposition**:

- `verified`: the exact original immutable ID, locator, header identity and
  sender passed independent inspection. Review `original_folder`, the resolved
  `original_memberships`, the current memberships in `state`, and
  `membership_changed`. A collection change is explicit old-to-current evidence,
  not identity replacement. Review the complete exact inspection `state`, its
  `read_ref`, `read_outcome` and `source_validated` stamp. No enumeration pair or
  presentation-change flags are fabricated. Names and addresses are preserved
  exactly, not used to merge identities or infer ownership.
- `unavailable`: that exact original reference returned a correctly associated,
  recognized Graph item-not-found response. This proves only inaccessibility
  through that reference at observation time, not deletion or archive absence.
  The original row remains history, without invented membership or revision.
- `historical`: the original row was already removed. Its history still requires
  owner attribution, including when there are no live rows.

Output limits affect presentation only. The shared output writer retains the
complete report in the private overflow file named on stderr. Review that file
when output overflows. Directories/files use `0700`/`0600`; keep reports outside
the repository and do not publish account or mail identifiers. A blocked or
incomplete run produces no executable proposal digest.

Only if the owner knows that **all** history belongs to the displayed account,
including every unavailable row, and approves every displayed old-to-current
membership change and exact inspected state, obtain
specific approval for local attribution:

```text
mailpipe account reconcile --execute --confirm-account <account> --confirm-history <history-digest> --confirm-reconciliation <reconciliation-digest>
```

Use the exact reviewed values. Version 9 uses method
`graph-exact-original-inspections-v1` and coverage `graph-original-references-v1`.
`--confirm-reconciliation` accepts the stable approval `reconciliation_digest`,
never `audit_digest`. Execute regenerates all exact-reference evidence under
exclusive lifecycle ownership. A changed inventory or inspected approval input
requires a new preview and review. The fresh audit binds actual read context and
may differ from preview; the CLI has no prior audit input. Both digests must match
THIS execution's frozen owned snapshot at final transaction review. An editable
report cannot supply remote proof. Neither inaccessibility, matching sender,
message content nor sign-in establishes historical ownership.
At least one original live immutable reference must verify before newly
unavailable references can be accepted as owner-confirmed history.

## Resource bounds

Default recovery admits at most 1,000,000 original live references and 256 MiB of
encoded complete inspected states. Exceeding a bound fails the entire pass;
there is no successful truncation. The cooperative 30-minute remote-read budget
honors an earlier parent deadline. It is not a hard deadline for the entire
command: history preparation, verified backup and SQL retain their parent context.
Report `requested_references`, `completed_references`, `inspection_batches` and
`state_bytes` describe this acquisition. Logical batches exclude transport retry
attempts. Existing finite retry rules remain; there is no global retry-until-quiet.
History inventories, copies, receipt serialization and backup space still scale
with history. Multiple exact lookups are not an atomic mailbox snapshot.

## Results and limits

A completed reconciliation permanently binds the existing mirror and records an
atomic receipt with the source/approval/audit digests, verified backup reference/digest, coverage,
confirmations and per-message dispositions. All original message numbers, Graph
IDs, legacy columns, bodies, verdicts, filing provenance, plans and action IDs
remain. Unavailable rows have no current membership and are excluded from active
inventory, but remain accessible by original message number and in history.
Their immutable identity reservation is retained. A later normal sync observing
that same ID reuses its original number; another ID gets a distinct number even
when headers/content match.

After routing is saved, normal full sync can run. It uses fresh checkpoints;
legacy delta links are not adopted. Historical pending plans remain readable but
cannot execute. Create and review fresh plans. Approval for `apply --execute`,
`rules apply --execute`, `save --execute`, and private policy changes remains
separate.

`account reconcile --json` displays the committed receipt offline when routing
selects the bound account. The receipt describes the reconciliation observation,
not the mailbox's current state after later syncs. If attribution committed but
routing resolution/save failed, the error reports partial local completion.
Repair the private configuration for the same account and retry execute with the
same three confirmations. This verifies the committed receipt and repairs routing;
it does not replay reconciliation, replace the mirror or rebind it.

Incomplete enumeration, authorization/read failures, canceled reads, malformed
responses, wrong request association, ambiguous error envelopes and exceeded
safety limits never become absence evidence. Resolve the read failure before a
fresh preview. Structural conflicts report a stale/blocked result; preserve the
mirror and review original identity evidence. Fixed `phase=inspection` categories
include known cancellation/deadline, read failures and selected HTTP responses.
`invalid-reference-response` denotes unsupported selected-state or 404 envelope
structure, not a provider cause. No raw cause text is printed.

The explicit legacy diagnostic modes additionally report `enumeration`,
`consistency` and `completion` phases. Their list/paired semantics below do not
apply to default reference-scoped attribution.

HTTP categories describe responses, not mailbox ownership or sign-in state.
In particular, `not-found-http-404` in a failed pass is not absence evidence or
an executable disposition, and an authorization response does not identify the
account or credential problem. These labels contain no message/account IDs,
local numbers, provider codes/messages, URLs or exception text. They add no reads,
retries or proof; every failed pass still has no proposal digest.

Legacy diagnostic enumeration failures distinguish five fixed categories. `removed-marker-null`
means the decoded record contained an explicit null `@removed` member;
`removed-marker-object` means a present object, without validating its reason;
`removed-marker-invalid-shape` means another JSON token shape. All remain
rejected. Syntactically malformed responses and missing/null selected metadata
keep their earlier error categories, even when a removal member is present.

`duplicate-id-within-page` and `duplicate-id-across-pages` describe recurrence of
the exact case-sensitive decoded ID among successfully mapped records on the
same or a previous page. Removal classification takes precedence if both apply.
Existing metadata, cancellation and resource checks keep their precedence. These
labels disclose no IDs, page numbers, tokens or values, add no reads/retries, and
prove neither the provider's cause nor deletion, ownership or complete coverage.
They do not permit deduplication or import delta-query replay rules into list
pagination. Selected-original diagnostics retain their generic fixed acquisition
failure message; the inner category need not appear in that mode.

## Private sender-address diagnostics

These explicit diagnostic modes retain the older full-list/paired-observation
protocol. They never reach successor store review or produce a proposal, even
when the legacy collection succeeds. A legacy consistency failure can report
`fields=sender-address` with a fixed string relationship. This does not identify its cause or establish which sender is
correct. The strict checks remain in force. If an owner needs to inspect the
conflicting record, an unconfirmed preview can explicitly request a raw private
artifact:

```sh
mailpipe account reconcile --sender-address-diagnostic-out /absolute/private-directory/sender-diagnostic.json
```

The directory must already exist, belong to the current user and have mode
`0700`. Use a dedicated directory outside checkouts, separate from the selected
database and configuration directories. The absolute destination must be new;
existing files, links and other objects are refused. The command does not create
or repair directories. The flag cannot accompany `--execute` or any confirmation
flag, and cannot display a committed receipt or repair routing.

Without a selector, only the first admission failure can produce an artifact,
and only when it contains a rejected enumeration-versus-inspection sender-address
pair. An earlier unrelated failure can prevent that diagnostic. Other failures,
including a historical original-sender mismatch, do not produce this artifact.

To inspect one intended original independently of admission failures, add
`--sender-address-diagnostic-n <positive-original-n>` to the command above. Use
the exact local number from the current unbound history, in canonical positive
decimal form. Unknown, removed or ambiguous originals are refused. This mode
completes the same enumeration followed by ordered inspection of **all** original
references; it adds no requests and does not perform a selected-only lookup.
Read/authentication failures, incomplete responses, unsupported provenance or an
unavailable selected reference prevent the artifact. A recognized exact-item
not-found for a different, unobserved original is only a read result here, never
an absence disposition or historical proof.

Selected mode does not evaluate reconciliation admission or historical identity.
It can save equal sender addresses or a pair with unrelated metadata differences;
neither grants acceptance. It always exits **runtime 1**, even when the private
file is saved successfully, with empty stdout and no proposal. Review the private
diagnostic rather than blindly retrying or interpreting the failure as a request
to execute reconciliation. Default preview admission remains strict.

The format-3 JSON contains the selected account, original local number, requested
immutable ID and both exact decoded mapped sender-address strings. Each
`enumerated_origin` / `inspected_origin` also retains the decoded Graph `from`
and `sender` address strings with `missing`, `null` or `present` field presence,
the fixed `selected_source` (`from`, `sender` or `none`), and
`mapped_sender_matches_wire` / `recipients_match_wire` integrity results.
These views come from the selected fields in the actual response, independently
checked against the adapter state at decoding. The chosen sender address is
checked again at collector and writer handoffs. To/Cc values and all names remain excluded.
From takes precedence when present, even with an empty address; Sender is only a
missing/null From fallback. A source field is not an RFC header or account proof.

The artifact marks enumeration complete but reconciliation incomplete, with no
historical identity validation. Its fixed scope distinguishes
`first_failure_sender_disagreement` (status `blocked`) from
`requested_original_observation` (status `diagnostic_only`). The latter explicitly
sets `describes_first_failure=false`, `admission_evaluated=false` and
`historical_identity_validated=false`. It does not claim to describe the first
failure. Existing format-1/2 files are not imported or overwritten. No format
contains a proposal or approval. In first-failure preview mode without a selector,
origin failures can display fixed `unsupported-provenance`, `provenance-mismatch`
or `provenance-state-changed` labels, not a guessed source or accepted sender.
Selected mode instead wraps acquisition failures in a fixed message that the
observation could not be acquired and no reconciliation proposal was generated.
The underlying cause identity is preserved, but its inner labels need not appear
in CLI output. Selected mode exits runtime 1 with empty stdout and no proposal;
no raw details are printed. Only the explicit option collects these extra origins;
normal inspection and acceptance stay unchanged.
Acquisition has not established historical identity or admitted all originals;
these observations are untrusted diagnostic data, not ownership evidence.

Retained original-message provenance has a separate 16 MiB aggregate raw-field
budget; exhaustion invalidates the pass (`provenance-limit` in first-failure
preview mode) without omitting records or producing a proposal. It adds no mailbox
requests, does not retain every enumerated message, and is not a constant-memory
guarantee. Selected mode retains extra origins only for the requested original.
The private file is exclusively created with mode `0600`, bounded to 1 MiB and
verified in full. No values are shortened, normalized or redacted in this
explicitly raw artifact. Invalid encoding, excessive size, write/readback errors
or uncertain cleanup fail without a saved-success hint. A late close or cleanup
failure can leave a private file; inspect the destination before retrying and
choose a new destination. Existing files are never overwritten. Normal failure
stdout remains empty and stderr contains only fixed diagnostic status labels,
without the path or raw observations. Even a saved diagnostic leaves the command
failed, with no executable reconciliation digest or attribution.

Inspect it only in a secure local viewer that treats JSON strings as data. Mail
fields can contain secrets, URLs, terminal controls or malicious instructions.
Do not execute their contents, print them directly to a terminal, or paste/upload
the file to agents, cloud services or issues. Safeguard the artifact after
inspection or remove it through owner-managed local cleanup; the command does
not delete successful reports and makes no secure-erasure guarantee.

A no-anchor result requires restoring access to an original immutable reference
or obtaining a separately approved evidence policy. It does not direct you to
prebinding full sync or automatic sign-in. Unknown ownership remains a stop.

Enumeration is limited to the signed-in primary mailbox exposed by `/me/messages`,
including Deleted Items. It is not an atomic mailbox snapshot, archive/recoverable
store survey, proof of permanent deletion, or proof of ownership of absent items.
Enumeration and direct-inspection contradictions require fresh evidence. Every
original live ID is inspected independently using read-only Graph batches.
Different IDs sharing historical header/sender evidence remain separate and do
not block this exact-ID verification. Their resemblance establishes neither
ownership nor continuity; a counterpart cannot substitute for an original lookup.
Archive moves and export/reimport can change immutable IDs.
See Microsoft's [message-listing contract](https://learn.microsoft.com/en-us/graph/api/user-list-messages?view=graph-rest-1.0)
and [immutable-ID guidance](https://learn.microsoft.com/en-us/graph/outlook-immutable-id).

### Identity, membership and older reports

Current reference-scoped attribution requires the exact immutable reference and
original historical header/sender checks. Review the exact inspected state and
old-to-current memberships under the version 9 contract in
[ADR 0012](adr/0012-reference-scoped-reconciliation.md). Strict `account bind`,
ordinary sync and action preconditions retain their existing comparisons. A
missing historical source-folder mapping remains blocked; none is guessed.

Committed versions 1–8 describe the older full-list/paired-observation protocol.
Their presentation flags and pairing rules are documented in
[ADR 0010](adr/0010-exact-id-reconciliation.md), and version 8's approval/audit
separation in [ADR 0011](adr/0011-reconciliation-approval-audit-digests.md).
Those rules do not define new proposals. Pending versions 1–8 cannot authorize
version 9; obtain a fresh preview. Committed receipts retain byte-immutable
offline display and exact stored-confirmation routing repair. Missing old audit
identities are not reconstructed or invented.

Older saved first-conflict diagnostic reports describe an incomplete observation,
not a proposal. Their bounded original/observed fields and omission counts are
useful for locating private history, but cannot establish identity, ownership,
complete enumeration or execution authority. Do not derive confirmations from
such a report. Generate a fresh complete preview with the current binary.

Output limits affect display, never evidence completeness. Nested report fields
use the shared recursive secret redactor; terminal controls are escaped for
display. Overflow files remain private, untrusted metadata. Metadata failures report fixed field labels and positional address/name
relationships, without values. Address labels are `exact`, `case-only`,
`trim-only`, `trim-and-case`, `different` or `invalid-utf8`; recipient shape
mismatches use `representation-different` or `cardinality-different`. Encoding
is checked first for scalar addresses; recipient shape precedes encoding.
Otherwise exact, untrimmed case, trim-only and combined comparisons take
precedence in that order. Names use exact equality only. `different` can mean
order or content differences, not semantic mailbox identity. Trim labels explain
a rejection and never permit trimming. Errors do not echo message identifiers,
subjects, bodies or raw provider responses.

The collector's [paging and evidence limits](../pkg/graphmail/reconciliation.go)
and the Graph client's [response bounds and retries](../pkg/graph/graph.go) are
specified in their implementations. Reaching a limit is incomplete evidence
requiring a reviewed resource plan, not permission to truncate or narrow
verification. Every authenticated request retains the pinned immutable subject
and immutable-ID preference.

## Storage and preparation cost

The [schema and migration implementation](../pkg/store/migration.go) defines the
current version and recognized upgrade paths. The existing reconciliation
receipt tables use the verified backup/migration mechanism; the versioned JSON
review format requires no schema change. Unknown shapes, extensions and future
versions remain refused. Ordinary bound stores gain empty
receipt tables without a new attribution. Older binaries refuse newer schemas.
Retain backups and stop older nonparticipating writers before maintenance; never
delete lifecycle sidecars or manually edit SQLite rows.

The [binding implementation](../pkg/store/account.go) retains full logical history
digest checks. A current-schema preview performs one full digest. Preparation
plus execute retains four digest boundaries: preparation, authoritative prebinding
source, verified immutable backup, and transaction source. Migration has additional
source/backup and original-data checks and is measured separately. Execute performs
one remote evidence pass; it does not repeat an extra preview inspection inside
the same invocation. The preceding operator preview remains a separate operation.

Digest traversal reuses prepared statements only within each table of one
invocation. It preserves the original schema/type/NULL-sensitive hash byte stream,
sparse row ordering, cancellation and the bounded cell chunks defined in the
[digest implementation](../pkg/store/migration.go). It does not cache
a digest across operations, omit a safety pass, or load whole large cells.
Preparation still scans the complete history. SQLite caches, inventory/proof
lists, receipt serialization and ID sets add memory beyond the chunk size.
Cumulative allocation is not peak resident memory.

Reproduce invented-history measurements with:

```sh
go test ./pkg/store -run '^$' -bench '^BenchmarkLegacy(History|Migration|Reconciliation|Cancellation)$' -benchtime=1x -count=2 -timeout=30m
go test ./pkg/graphmail -run '^$' -bench '^BenchmarkGraphLegacyEvidence$' -benchtime=1x -count=2
```

The [store benchmark source](../pkg/store/reconciliation_performance_test.go)
defines the invented fixture sizes, body distribution, verdicts and action
history. It also defines the phase timer boundaries. The
[Graph benchmark source](../pkg/graphmail/reconciliation_performance_test.go)
defines its in-process read-only transport, missing-reference fixture and counted
read envelopes, batches and exact inspection subrequests. Without retries,
inspection uses one batch per adapter batch limit rather than one HTTP request
per original; it still looks up every original independently. The legacy benchmark includes enumeration; default reference acquisition does not.
Full history digests, backups and receipt serialization retain their existing costs. Network latency is excluded. Timings are observations, not CI
thresholds or production capacity guarantees.

The measurements below were collected on 2026-11-04 **before** removed-history
preparation was narrowed to message numbers and Graph IDs. They document that
pre-projection experiment, not current preparation or binding timings. The
digest and migration algorithms were not changed by the projection correction.
Use the linked benchmark sources to measure the current implementation.

### Historical preparation and digest experiment

Measurements used Go 1.27.3 on darwin/arm64, Apple M1 Max. The original
algorithm was measured once and statement reuse twice on the same version-2
invented fixture shape. Other verification work overlapped some samples; these
are observed timings, not a controlled latency guarantee. Fixture files were
72,359,936 bytes (10k messages) and 573,444,096 bytes (100k messages).

| Phase/history | Original seconds | Statement reuse seconds (two samples) | Original allocated bytes/op | Reuse allocated bytes/op | Original allocations/op | Reuse allocations/op |
|---|---:|---:|---:|---:|---:|---:|
| Prepare / 10k | 11.370628 | 5.279848–5.295800 | 822466416 | 835770792–835771512 | 20446595 | 21273856–21273900 |
| Prepare / 100k | 106.957466 | 44.586061–53.045138 | 7938064024 | 8070410232–8070611896 | 204455736 | 212715919–212716697 |
| Digest / 10k | 10.918107 | 5.122999–5.166813 | 807404840 | 820716216–820725168 | 20221793 | 21049137–21049173 |
| Digest / 100k | 154.343143 | 42.459866–43.108305 | 7769942096 | 7901784952–7901803744 | 202252890 | 210511101–210511184 |

The digest instrumentation counted 826,445 direct query calls at 10k and
8,259,545 at 100k with the original algorithm, with no explicit `PrepareContext`
calls. Reuse counted 135 explicit preparations and 19 direct metadata queries
per digest at either size. These are counted SQL API calls, not a claim that the
original driver performed zero internal preparations. Prepared statement
executions still scale with rows, columns and chunks. Cumulative allocations
increased slightly even though these preparation timings improved. No source,
backup or transaction digest boundary was removed.

The historical enumeration-based in-process Graph collector measured 0.299–0.320 seconds at 10k
references (136385696–136738696 allocated bytes, 1513494–1513835 allocations)
and 2.430–2.556 seconds at 100k (1322105944–1323273808 allocated bytes,
15135337–15135570 allocations). It made exactly 10,000 inspections plus 50 pages
at 10k, and 100,000 inspections plus 500 pages at 100k: 10,050 and 100,500
transport read calls respectively. Each fixture had one unavailable reference.
These timings exclude network latency and retries. An operator preview and a
later execute each collect fresh evidence; large real histories can therefore
require substantial network time as well as local preparation and backup space.

### Historical migration, binding, cancellation and memory

| Phase/history | Seconds (two samples) | Allocated bytes/op | Allocations/op |
|---|---:|---:|---:|
| Version-1 migration / 10k | 14.081702–14.857903 | 3938504992–3938599920 | 89166156–89166709 |
| Version-1 migration / 100k | 110.755951–119.219737 | 38156723568–38157391296 | 891630059–891633177 |
| Reviewed binding / 10k | 12.986785–13.564907 | 2800111072–2800138712 | 65919779–65919820 |
| Reviewed binding / 100k | 106.586657–113.954031 | 27091360696–27091375376 | 659134574–659134691 |

In that experiment, preparation cancellation requested after 10 ms returned in 0.010919–0.012417 seconds, including cancellation and cleanup. This is measured latency, not a timing assertion.

The timed store benchmark command reported `516243456` as its maximum resident set size (the raw macOS `/usr/bin/time -l` value). That aggregate covers fixture setup, build/test processes and all phases. It is not steady-state application memory and does not imply constant process memory. The raw timings, allocation counts and preparation counts above deliberately retain the costs of all safety checks.
