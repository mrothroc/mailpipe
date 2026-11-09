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
It inventories the complete database, enumerates current primary-mailbox metadata,
and inspects every original live immutable reference. It fetches no bodies.

Review the immutable account, complete history digest, reconciliation approval
digest, separate observation audit digest,
coverage and observation times, counts, and **every message disposition**:

- `verified`: the exact original immutable ID, locator, header identity and
  sender passed independent inspection. Review `original_folder`, the resolved
  `original_memberships`, the current memberships in `state`, and
  `membership_changed`. A collection change is explicit old-to-current evidence,
  not an identity replacement; every change requires owner review. If
  `sender_address_disagrees`, `sender_casing_changed`, `recipient_casing_changed`,
  `recipient_name_changed` or `sender_name_changed`
  is true, compare the exact `enumerated_state` with inspection `state`. Sender and recipient
  display names and sender/indexed-recipient address casing are separate reviewed
  differences. A noncase sender disagreement selects inspection only when its
  sender/header still pass the original history checks and both source mappings
  are validated. It does not declare aliases equivalent or establish ownership.
  `sender_address_disagrees` and `sender_casing_changed` are mutually exclusive.
  Recipient name and casing flags remain independent. Recipient
  address relationships, positions, shape, duplicate multiplicity and other
  metadata stay strict.
  Review the complete pair, including private overflow output.
- `unavailable`: a completed enumeration did not observe this exact ID, and a
  subsequent exact lookup returned Graph's recognized item-not-found response.
  The original row remains history, with no invented current membership/revision.
- `historical`: the original row was already removed. Its history still requires
  owner attribution, including when there are no live rows.

Output limits affect presentation only. The shared output writer retains the
complete report in the private overflow file named on stderr. Review that file
when output overflows. Directories/files use `0700`/`0600`; keep reports outside
the repository and do not publish account or mail identifiers. A blocked or
incomplete run produces no executable proposal digest.

Only if the owner knows that **all** history belongs to the displayed account,
including every unavailable row, and approves every displayed old-to-current
membership change, sender-address disagreement, address casing pair and
sender/recipient display-name pair, obtain
specific approval for local attribution:

```text
mailpipe account reconcile --execute --confirm-account <account> --confirm-history <history-digest> --confirm-reconciliation <reconciliation-digest>
```

Use the exact reviewed values. For version 8, `--confirm-reconciliation` accepts
the stable `reconciliation_digest`, never `audit_digest`. Execute regenerates and
validates all evidence under exclusive lifecycle ownership. A changed inventory
or inspected approval input requires a new preview and review. Already-permitted
enumeration presentation differences may produce a fresh audit while approval
stays identical. The CLI has no prior audit input and does not claim to detect
that difference. Within one execution, both digests must match its frozen owned
snapshot at final transaction review. An editable report cannot supply remote proof. Absence, a matching sender,
message content, and a successful sign-in do not establish historical ownership.
At least one original live immutable reference must verify before newly
unavailable references can be accepted as owner-confirmed history.

## Results and limits

A completed reconciliation permanently binds the existing mirror and records an
atomic receipt with the source/approval/audit digests, verified backup, coverage,
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
pages, unsafe or cyclic pagination, duplicates and exceeded safety limits never
become absence evidence. Transient failures report a read-failure result: resolve
the cause and retry a preview. Structural conflicts report a stale/blocked result:
preserve the mirror and request owner review of the original identity evidence.
Incomplete-evidence errors include fixed `phase` and `category` labels on stderr.
`enumeration` identifies the primary-mailbox scan, `inspection` the exact-original
lookup pass, `consistency` a disagreement between those observations, and
`completion` the final cancellation check. Categories distinguish known typed
cancellation/deadline, response limits, malformed responses, transport/read
failures and selected HTTP response classes. Unknown causes remain
`unknown-read-failure` or `unknown-api-response`; no raw cause text is printed.
For example, `phase=consistency; category=metadata-changed` requires fresh
observation and review, while `phase=inspection; category=throttled-http-429`
reports a read that still failed after the existing bounded retries.

HTTP categories describe responses, not mailbox ownership or sign-in state.
In particular, `not-found-http-404` in a failed pass is not absence evidence or
an executable disposition, and an authorization response does not identify the
account or credential problem. These labels contain no message/account IDs,
local numbers, provider codes/messages, URLs or exception text. They add no reads,
retries or proof; every failed pass still has no proposal digest.

## Private sender-address diagnostics

A consistency failure can report `fields=sender-address` with a fixed string
relationship. This does not identify its cause or establish which sender is
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

A found original must match its exact immutable reference, header and sender.
A different collection is accepted only as explicit reviewed state in this
reconciliation path. Enumeration and inspection must agree on current metadata,
membership and any recorded revision, except reviewed same-ID sender and indexed
recipient display-name differences, sender/recipient address casing, and the directional sender rule
below. The name exception preserves both exact decoded observations;
changed names must be valid UTF-8. It does not normalize names or assert that two
display names identify the same person or owner. The casing rule is unchanged:
it uses Unicode simple case folding (`strings.EqualFold`)
without trimming, sorting, alias expansion or Unicode normalization. It does
not establish RFC mailbox equivalence. Original spellings remain in both states;
no address is globally lowercased. Strict `account bind`, ordinary sync and action-plan
preconditions retain their existing comparisons. An unavailable historical
source-folder mapping remains blocked; no name or collection is guessed.

`recipient_name_changed` reports any byte difference in names at corresponding
positions; `recipient_casing_changed` reports address spelling differences only.
Each changed name must be valid UTF-8. Empty, whitespace and control-containing
names remain exact untrusted strings in both raw states, without normalization.
The existing To-then-Cc order, nilness, cardinality and duplicate multiplicity stay
strict. Comparison never sorts or matches across slots. Indistinguishable
duplicate address slots have position-only comparison, with no inferred cross-slot
identity; review both full arrays. Names do not establish identity or ownership.

For the same authenticated account and exact immutable reference, recovery may
select inspection's sender address despite a noncase enumeration disagreement
only when inspection still passes the ORIGINAL historical sender/header checks.
Both observations must have independently validated source-to-mapping integrity;
false or unknown integrity blocks this exception. Changed addresses must be
nonempty valid UTF-8. The report retains both complete raw states and derives
`sender_address_disagrees` separately from case-only `sender_casing_changed`.
This is directional observation selection, not trimming, alias expansion, RFC
equivalence or permission to replace original history. All unrelated metadata,
recipient address positions/shape/multiplicity, current memberships and recorded
revisions remain strict. Default collection retains only integrity stamps, not raw source traces.

New proposals and receipts use JSON version 8. `reconciliation_digest` is stable
approval under domain `mailpipe-legacy-reconciliation-approval-v8`: exact account,
complete history digest, coverage, folder mappings, identities/dispositions,
original memberships/change flags and the complete inspected states. Every
inspected metadata spelling, revision and membership remains approval-bound.
Enumeration-derived flags and optional pair presence are excluded, without
relaxing validation of that evidence.

`audit_digest`, under `mailpipe-legacy-reconciliation-audit-v8`, binds that computed
approval context, observation times/counts and full raw items with retained pairs
and derived flags. Missing enumeration state remains absent, never fabricated;
metadata-equal cases retain no extra enumerated state. This is not a new
all-record raw capture. Review the
complete current report, including private overflow. An audit identifies untrusted
observations and cannot grant approval. Invalid evidence produces neither digest.

A version-1 through version-7 pending proposal cannot execute new binding, even
without differences; obtain a fresh preview. Committed version-1 through version-7
receipts remain readable offline and retain exact STORED-confirmation routing
repair, without rehashing/reinterpreting or rewriting receipt/item bytes. They
have no invented audit digest. See [approval and audit](adr/0011-reconciliation-approval-audit-digests.md)
for the precise separation and [exact-ID reconciliation](adr/0010-exact-id-reconciliation.md)
for unchanged admission/history boundaries.

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
per original; it still looks up every original independently. Enumeration, full
history digests, backups and receipt serialization retain their existing costs. Network latency is excluded. Timings are observations, not CI
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

The separate in-process Graph collector measured 0.299–0.320 seconds at 10k
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
