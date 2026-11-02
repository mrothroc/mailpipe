# Operations

Engineering evidence, owner-attended acceptance and later publication are separate.
All examples are blank or invented. Scans are manual; no schedule is installed.

## 1. Scope and capability boundaries

| Capability | Graph | Yahoo read-only pilot |
|---|---|---|
| Sync, local listing and complete bounded body/MIME reads | supported | supported |
| Configured stages and local reviewed plans | supported | supported |
| Reviewed trash and managed native sender rules | supported with specific approval | unavailable |
| Archive/native filter parity | provider-specific, never inferred | unavailable |
| `apply`, `rules apply`, `save --execute` | specific owner approval required | refused before effects |

Yahoo scan-time policy is local intent, not server-filter installation. Dry save
checks a manifest; it does not imply Yahoo PDF execution support. Gmail implementation,
Yahoo mutations/native filters and unattended execution are deferred. A test named
`TestYahooReadOnlyPilot` is synthetic, not proof of a live pilot.

The publication lifecycle, permanent account/provider binding, complete-content
limits and protective review/keep verdicts remain unchanged; see [architecture](architecture.md)
and ADRs 0001–0006. Capabilities do not confer owner approval.

## 2. Provider setup, attribution and processing permissions

Absent provider preserves Graph configuration. Follow the Microsoft 365 registration
instructions in [README](../README.md). Yahoo requires a separate private configuration
and database for each exact principal. Inspect `config init --help` first. In the
owning user session, after applicable consent:

```sh
mailpipe config init --provider yahoo --login alex@example.com
mailpipe auth login
mailpipe account show
mailpipe account capabilities
mailpipe auth status
```

The login is invented. The owner supplies their actual login privately, never in
chat. `auth login` hides terminal input; approved nonterminal secure producers need
`--password-stdin`. No password belongs in argv, environment, configuration or a file.
`auth status` reports stored readiness, not fresh remote authentication proof.
Actual account/config/auth outputs contain private routing and must stay private.

The optional `--endpoint` accepts only the [implicit verified-TLS grammar](adr/0006-yahoo-read-only-setup.md).
Omitted endpoint uses the provider default; omitted port means 993. DNS ASCII case
and explicit default port are canonicalized; login case/spaces remain exact opaque
bytes. No URI, insecure TLS or guessed principal normalization is available. If role
evidence is missing, private `imap.role_mappings` can name exact existing selectable
paths with `sent`, `trash`, `junk` keys. Discovery validates them and creates no folder.
Yahoo requires acknowledged full-view UIDONLY support; capped views fail closed.

Fresh Graph/Yahoo setup disables model and external subprocess transfers separately.
Only explicit account-scoped authorization through `config processing --account <id>`
with `--models=true` and/or `--external-stages=true` enables those scopes. This pilot
procedure enables neither. Existing Graph configurations without a processing record
retain configured legacy behavior; explicit records apply only to their exact account.
See [stages](stages.md) and [hooks](hooks.md). Trusted hooks retain OS permissions;
metadata-only subprocesses also require consent. No mail should be returned to an AI
as a supposed local-only inspection.

## 3. Migration, preflight and rollback safeguards

Changing endpoint, login or provider requires a different database; there is no
rebind. Same-principal password rotation preserves history. Restoring the exact old
configured tuple can reopen its original database. Service-side reassignment of the
same login is not immutable identity proof: use a different database. Headers and
matching message content cannot establish account continuity.

Before migration or recovery, stop older nonparticipating clients and close database
sessions. Preserve the verified backup and SQLite recovery siblings. Do not raw-copy a
live WAL database, delete lifecycle lock files, replace a bound database or downgrade
its schema. Failed migrations roll back; uncertain outcomes require reconciliation.
Offline restoration uses a fresh private destination and the existing verified restore
API, with exclusive lifecycle ownership; there is no general CLI restore command to
invent. Rolling back a binary does not prove compatibility with a newer database.
Retain the current recovery set and verify version compatibility before opening it.

### Attributing an existing Graph mirror

Existing configuration without a provider still uses Microsoft 365. A new empty
mirror binds to the authenticated immutable mailbox subject on its first sync.
An existing historical mirror requires an explicit attribution step:

1. Stop older nonparticipating programs and close other database sessions.
2. Run `mailpipe account bind` with the intended private configuration and database.
   This performs the existing backed-up schema opening, then verifies every live
   message against the authenticated account. It reports an inventory digest;
   it does not bind the history or write account routing.
3. Review the account and confirm that **all** removed messages and historical
   verdicts, plans and actions belong to it, even if there are no live messages.
4. With that approval, run `mailpipe account bind --execute --confirm-history <digest>`.
   Changed inventories or missing/mismatching live evidence require reconciliation.
5. Create and review fresh numbered plans. Historical pending plans lack bound
   execution evidence and cannot be applied. Run a fresh sync; old delta links
   are not adopted.

This changes local attribution/configuration, not server mail. Approval for
`apply`, `rules apply` and `save` execution remains separate. Binding cannot switch
an existing database to another account/provider. Other accounts require separate
configuration and database files.

If binding succeeds but saving routing fails, retain the database and its backup.
Restore a valid private configuration for the same authenticated account and rerun
`mailpipe account bind --execute` to verify the existing binding and repair routing.
Do not replace or reassign the database. Local commands remain offline once routing
is available; cached empty bodies also need no sign-in.

An uncertain move or failed action audit requires reconciliation before another
execute attempt. Failed sync cleanup is reported separately; close sessions before
exclusive abandoned-staging maintenance. No command automatically discards another
session's staging or weakens lifecycle locks.

## 4. Stage 1: synthetic conformance and candidate delivery

Stage 1 is automated engineering/CI. Inputs are public source, invented fixtures,
local test servers and fake credentials/models. It never requests or uses real
mailbox, model or Keychain credentials. Default `make check` never installs into a
personal Brew prefix, Cellar or tap.

Run the full release gate and explicitly discover/run the provider conformance and
acceptance tests described in [development](development.md). Existing Graph behavior,
account isolation, full MIME/body boundaries, read flags, protection verdicts and
hook/filing/security regressions remain required. Synthetic success is not live
acceptance and cannot fill Stage 2 checks.

The required sequential `candidate_formula` CI job validates the exact PR head/event
commit on `macos-15`, including another full `make check`, then runs the checked-in
candidate harness. Its `git archive` commit, SHA256, loopback download and formula
checksum must agree. It builds from source, runs `brew test` and absolute installed
help/examples/synthetic Graph config plus Yahoo config/capability/default-off smoke.
It does not run installed Graph capabilities, auth/login/status/sync or models.
The Graph capability command requires a real authenticated subject; it is not a
synthetic setup smoke command.

Real Brew tests use only a disposable GitHub-hosted job's default prefix. Temporary
HOME alone does not isolate Cellar/taps or the OS Keychain. Ordinary local harness
installation is refused before Brew. A held `127.0.0.1:0` server serves only the
exact archive, with a bounded readiness handshake. Candidate-only trust, owned
process teardown and archive/tap/HOME cleanup are mandatory on success, failure and
catchable cancellation. The normal tap/tag remains unchanged.

Review actual exact-candidate Actions logs for install, help/examples/config results,
checksum, supported runner/provenance, timing and teardown. The declared validation
target is `macos-15` only; successful evidence is required before calling it tested.
Other macOS versions/architectures are unverified. No successful candidate install
or completed pilot is established merely by this runbook. Abrupt termination without
final records is incomplete, not successful zero-time evidence; repeat the same
reviewed candidate on a fresh runner. See [ADR 0007](adr/0007-provider-acceptance-and-delivery.md).

## 5. Stage 2: owner-attended live checks and private receipt

Stage 2 is a distinct interactive session **after** a tested, reviewable candidate
and specific account-owner consent. Engineering/planning permission does not grant
live mailbox/auth/Keychain/model access, publication or deployment. Missing owning
session/access is a release-acceptance blocker, not a waived check. CI never runs
this stage. The owner enters credentials only through their secure CLI/session.

Before requesting consent, prepare the instructions below, the candidate identity,
Stage 1 results and recovery procedure for review. Consent must cover the selected
account's read-only sync/content, private local storage and bounded negative checks;
model/subprocess processing remains off. A separate opaque consent reference lives
with the private record, not owner/account identity. No raw mail, protocol transcript,
login, endpoint, message ID, private folder/policy/classifier details are required.

Choose an explicit absolute private root outside **every** repository/worktree and
Git common directory. Its existing directory must be owned and mode 0700; all receipt
files are exclusive regular single-link 0600. Canonical paths must have no symlink
components, unsafe writable ancestors, repository ancestors or alias/race ambiguity.
Run the acceptance helper from the reviewed source; it checks worktree/common-dir
inventory and unrelated enclosing Git/bare repositories and refuses unsafe paths.
It never chmods/adopts existing unsafe files. Stable private namespace against a
hostile process running as the same user is an explicit assumption.

```sh
# The owner chooses these values privately; the root must already exist with mode 0700.
go run ./tools/acceptance receipt-init --private-root "$PRIVATE_ACCEPTANCE_ROOT" --candidate "$CANDIDATE_COMMIT"
```

This creates only pending `receipt.json` and `consent.json`, never runs mail commands
or proves consent. If the second creation fails, initialization remains an error
and retains the receipt and existing artifacts for private review. It never deletes
a current pathname as rollback: a concurrent editor may have replaced or changed
that file. Quiesce writers and privately inspect partial state before an owner
chooses any removal/retry; another init will refuse existing files. A standardized
blank/invented receipt has this shape (zeros are
invented placeholders, not actual counts):

```json
{
  "version": 1,
  "candidate_commit": "",
  "provider": "yahoo",
  "platform": "macos-15",
  "consent_reference": "",
  "checks": {
    "consent": "pending", "stage1": "pending", "initial_sync": "pending",
    "repeated_sync": "pending", "incremental_change": "pending",
    "complete_readable_body": "pending", "full_mime_transport": "pending",
    "read_flags_unchanged": "pending", "account_isolation": "pending",
    "execute_apply": "pending", "execute_rules": "pending", "execute_save": "pending",
    "processing_disabled": "pending", "recovery_reviewed": "pending"
  },
  "aggregate_counts": {"initial": 0, "repeat": 0, "incremental": 0},
  "stage1_technical": {
    "archive_hash_verified": false, "installed_help": false, "installed_examples": false,
    "graph_config": false, "yahoo_config": false,
    "yahoo_read_only_capabilities": false, "owned_cleanup": false
  }
}
```

Use the exact independently verified 40-hex candidate. `platform` may be the generic
`macos` for the owner session; the public declared CI baseline remains `macos-15`. Only `pending`, `pass`, `fail`
are valid check values. Private counts are nonnegative aggregate totals, never folder
labels or raw CLI outputs. The generated consent record has version, opaque 32-hex
`reference`, `owner_confirmed`, `read_only_sync`, `content_reads`,
`local_isolation_check`, `denied_execute_checks` booleans and ISO `date`; it starts
unconfirmed. The owner records applicable consent privately before live commands.
Neither file needs an owner name, signature/account identity or arbitrary notes.
Edit only private copies with a trusted editor that preserves protections; validators
refuse duplicate/unknown keys, null/wrong types, oversized input and unsafe links.

After specific consent, the owner performs these checks in their session:

1. Verify candidate binary/source and actual successful Stage 1 evidence. Select
   separate private config/db/out/stages/templates paths for the intended principal.
   Ensure the real HOME/mail credential-lock directory is private. Do not change HOME
   to pretend to become another OS user. Configure and authenticate as in section 2;
   privately inspect account/database identity, capabilities and readiness. Keep
   processing disabled. Do not copy outputs into chat/review/CI.
2. Record an independent read/unread baseline in the normal mail UI without opening
   selected unread messages; quiesce other clients where feasible. Run
   `mailpipe sync --folder inbox --full --json` and separately consented folders.
   Privately compare per-folder local/remote totals; record only aggregate counts
   and check results. Run ordinary repeated sync without `--full`, then a later
   incremental sync after independently observed naturally arriving changes.
   No-change repetition does not establish changed-incremental behavior: that check
   stays pending until observed. Do not manufacture changes by sending/moving mail
   without separate permission.
3. Privately inspect `mailpipe show <n>` without `--no-quotes`; if display overflows,
   inspect the full private overflow file. Verify complete readable content/tail and
   relevant trailing dates against owner-known content. `mailpipe show <n> --headers`
   drives a complete bounded MIME transport fetch before displaying the header block.
   It is not raw MIME export or proof of every decoded part. Combine guarded fetch
   success, owner body inspection and synthetic full-source integrity evidence; if
   insufficient/inconclusive, leave pending and request a further specifically
   approved owner-local check rather than silently adding an export seam.
4. Verify UI read flags after body/header reads and another sync. Other-client churn
   makes results inconclusive, not a preservation pass. Keep all IDs/content private.
5. With consent to a bounded local negative check, create a separate synthetic
   principal configuration and attempt sync against the already-bound pilot DB.
   It must fail account mismatch before credential/network effects; verify original
   binding/history unchanged. Never edit/rebind the real profile or use another
   account's secret. Declining this check leaves account isolation pending.
6. With specific permission for expected-denial probes at the reviewed candidate,
   check `apply <local-or-empty-plan> --execute`, `rules apply --execute`, and
   `save --manifest <nonexistent-private-file> --root <new-private-probe-target> --execute`.
   These must report unsupported nonzero before effects, even when empty. Optionally
   log out the same principal first to remove remote credential availability. Confirm
   no destination/server/flag effects; synthetic collaborator-call tests provide
   no-Keychain/no-provider-call proof. Unexpected success/effect stops acceptance
   immediately; never retry a mutation. Do not run models/hooks or enable processing.
7. Record results only after private review, retaining aggregate counts privately.
   Do not report a complete pilot until every required check is verified at the
   candidate and root has reviewed the authorized private evidence. Validators do
   not independently observe the mailbox or grant consent.

## 6. Failure, recovery and stop conditions

TLS/full-view/capability/mapping failures stop; do not weaken verification, guess
folders or accept a capped mailbox. Churn and transient errors retry only through
the existing bounded lifecycle. Failed cleanup or uncertain publication/action
outcomes require reconciliation before retry. Partial content is never successful
body/MIME evidence; protective review/keep verdicts remain authoritative.

Failed credential persistence leaves incomplete readiness. Repair with fresh hidden
input to `auth login` for the same principal; do not select another secret or a file
fallback. Endpoint/login change needs a different DB. Routing-save failure retains
binding: repair configuration only for the same principal. See migration safeguards
above. Cancel with Ctrl-C, retain private failure evidence and inspect errors locally.
Only owner-approved same-principal logout/disposal is allowed; never delete shared
locks or another account's Keychain items. No actual receipt is placed in the repo.

### Ordinary reviewed Graph operation

`sync` updates the mirror. Run configured stages in dependency order with an explicit
time window (`--since`) and cost caps for paid stages. Review `plan create` and
`plan show <n>`, checking protective classifications and the owner's private policy.
Run `apply <n>` as a check before separately approved `--execute`. Inspect `rules diff`
before separately approved `rules apply --execute`. Filing uses `file draft`, owner
review, then separately approved `save --execute`; see [filing](filing.md). These
Graph execution instructions do not apply to Yahoo. Read-only mail access in this
specific acceptance exercise still requires the Stage 2 account consent above.

A wrong Graph deletion may be recoverable in Deleted Items only while retained;
`actions <plan>` lists attempts privately. Unknown remote outcomes must be reconciled,
not replayed. Private policy/template changes require owner approval and backup.

## 7. Redaction, acceptance and later publication gates

Actual consent/receipts and summary proposals stay outside every repository/worktree.
Never transfer receipt content, actual counts, consent references, timestamps,
paths, identities, endpoints, private choices or arbitrary notes to public evidence.

```sh
go run ./tools/acceptance receipt-validate --private-root "$PRIVATE_ACCEPTANCE_ROOT" --candidate "$CANDIDATE_COMMIT"
go run ./tools/acceptance summary-proposal --private-root "$PRIVATE_ACCEPTANCE_ROOT" --candidate "$CANDIDATE_COMMIT"
```

The proposal is a NEW fixed struct written exclusively as private
`summary-proposal.json`, never stdout or a public path. Its allowlist is version,
independently verified candidate commit, fixed `yahoo`, fixed declared Stage 1
`macos-15`, the seven technical booleans above, and `pilot` (`pending` by default).
Private counts/choices cannot become an allowed key's string value or nested data.
After all checks and consent are privately reviewed, `--owner-reviewed` may propose
`privately_verified`; the tool requires all check statuses pass and the matching
confirmed consent scope. This flag does not itself authorize publication.

Before any transfer, the owner opens only the proposal, checks exact schema/values,
verifies candidate and technical evidence, confirms no private values/extra keys,
and approves that specific transfer. Root independently reconstitutes only allowed
technical results. Never attach/copy the original receipt, raw command output or
consent. Automated reserved-domain canaries and count-shaped tests complement this
manual review; a secret scan alone cannot detect personal preferences.

Final task/release acceptance requires actual specifically consented private pilot
completion, full checks, required exact-candidate installed CI and an approved scoped
code review at the clean candidate. Stage 1 alone is insufficient. Publishing a
normal release/tag/tap and deployment need later applicable approval. The ordinary
tap then receives the released tag/checksum and current help assertion and its normal
checks; no publication is required to test the pre-publication candidate.
