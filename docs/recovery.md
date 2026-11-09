# Recovery and account attribution

Preserve the database, its migration backups and private command output before
attempting repair. Do not put diagnostics containing mail or account identifiers
in public issues; describe the failure with invented examples.

## Diagnose the failed layer

Authentication, configuration routing and durable database binding are separate.
A working sign-in can coexist with a historical mirror that needs attribution.
An empty configured account or a generic timeout alone proves neither sign-out
nor loss of binding. Record the exact binary, private paths, command, exit status
and error. `account show` describes configuration; provider-specific `auth status`
semantics are documented in [setup](providers.md).

Use the authorized execution environment when diagnosing OS/Keychain/network
restrictions. A sandbox refusal is not evidence of a broken mailbox connection.
Never inspect or print the token cache to diagnose sign-in.

A first schema migration can take substantial time to verify and copy history.
Observe the running command and its eventual result before launching another.
Do not delete lifecycle sidecars, remove guards, kill unrelated sessions or open
an older nonparticipating binary against the database to bypass maintenance.
Stop other writers before exclusive maintenance or restoring a consistent backup.

## Attributing an existing Graph mirror

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

## Historical binding blocked by missing live references

A historical Graph mirror can require attribution before sync while strict
`account bind` refuses a stored live reference that is now unavailable. Use
`mailpipe account reconcile` in that same private context to preview complete
read-only evidence and review every disposition, exact inspected state and
old-to-current membership change. The default reader inspects every historical
live immutable reference without unrelated mailbox enumeration. Found states
still pass original sender/header and exact account/reference checks; no sender,
name, address or content correlation transfers identity. Version 9 approval binds
complete inspected state, identities, dispositions, method/scope and complete
history. A separate audit binds actual acquisition context and source-validated
outcomes. Fresh audit may differ, but mutation within THIS execution's owned
snapshot remains stale. No enumeration pair or presentation flags are invented.
Pending versions 1–8 cannot authorize new binding; committed versions 1–8 retain
byte-immutable offline display and exact stored-confirmation routing repair.
Each original immutable ID is verified independently; matching headers
on a different ID never replace it or transfer history. With specific owner approval,
its execute path requires exact account, full-history and reconciliation digests.
See [reviewed Graph reconciliation](graph-reconciliation.md) for the supported
workflow, evidence limits, preserved history, backups and partial routing recovery.

Even a recognized exact 404 means only reference inaccessibility, not ownership
or deletion. Wrong accounts, ambiguous identities, incomplete exact reads and an
unavailable inventory with no verified live anchor
remain blocked. Preserve the mirror and backup; restore original evidence or
request owner review. Do not force binding, manually mark rows removed, fabricate
account metadata, or repeat prebinding full sync. Historical pending plans still
need fresh inspection and review before execution.

## Isolated read-only survey during blocked recovery

If a current-mail survey is needed, use a separate private configuration copy and
new empty database, with the intended existing login and approved stages. Keep
private directories/files protected and outside the checkout. A fresh mirror can
establish its own account attribution; this does not repair, replace or rebind the
original mirror. Preserve the original files and describe the survey as isolated.

Apply the same explicit configuration/database overrides to every command and
supervising helper. Trusted hooks receive a minimal environment, not arbitrary
`MAILPIPE_*` overrides; a hook that invokes mailpipe itself must be explicitly
configured for the intended private context. Framework source callbacks use the
command's own provider context.

Local message numbers, attachment selectors, manifests and numbered plans belong
to that mirror. Never execute a temporary plan number against the default database.
A scratch mirror lacks the historical action journal used to derive managed
blocking rules: do not use its `rules diff/apply` as a replacement for canonical
managed-rule state.

If reusing classifications, inspect `import-verdicts --help` and check the proposed
import first. The importer keys on Message-ID and sender; it does not establish
remote identity for you. Require proven same-account/provider remote identity and matching
message metadata, with unique targets. Header Message-ID or sender equality alone
cannot establish identity. Preserve provenance; do not copy execution plans or
invent action/binding evidence. Any proposed execution still needs review in the
exact account/database context and the owner's approval.

## Interrupted or uncertain work

A command that failed may have completed some remote or filesystem effects. Read
its action log and structured outcomes before retrying; uncertainty is not rollback.
Reconcile moves, failed audit writes and partially published attachment batches
before another execute attempt. See [attachments](attachments.md) for extraction
outcomes and [filing](filing.md) for saved-state checks.

Deleted mail remains recoverable only while the server retains Deleted Items;
`actions <plan>` identifies recorded moves. Save durable records before cleanup.
For blank metadata, a full sync can repair rows only when the normal account and
lifecycle preconditions permit sync. It does not bypass historical attribution.

A rejected sender-address observation can be inspected only through an explicit
preview-only [private diagnostic](graph-reconciliation.md#private-sender-address-diagnostics).
Its format-3 selected-field origins distinguish Graph From/Sender selection from
the mapped observations without exporting recipients or names. Unknown provenance
fails closed. This owner-only raw file does not relax sender checks or provide a
reconciliation proposal. An explicit original-number selector can observe one
pair independently of earlier admission failures, without evaluating historical
identity or generating proof. It still reads all originals and always exits
runtime 1 with empty stdout, even when saving succeeds. Keep it outside checkouts and database/configuration directories; treat
its contents as untrusted private data, safeguard or remove it after secure local
inspection, and never upload it to agents or issue reports.
