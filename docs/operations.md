# Operations

How to run a scan by hand, and which steps need the owner. Scans are manual for now; a launchd schedule comes later ([PLAN.md](../PLAN.md)).

## What needs the owner's approval

Read-only and recording steps can run freely. Each of these changes mail, the server or the owner's files, and needs the owner's approval for that specific change:

| Command | Effect |
|---|---|
| `apply <plan> --execute` | moves the plan's messages to Deleted Items |
| `rules apply --execute` | rewrites the managed blocklist rules on the mail server |
| `save --manifest <file> --execute` | writes PDFs into `~/Documents` |
| changes to `~/mail/stages.json` or `~/mail/templates.json` | change what future scans do; back the file up first |

Without `--execute`, each of these commands is a check that changes nothing. Run it first and show the owner the result.

## A manual scan

1. **`mailpipe sync`** updates the mirror. The local count must equal the server's for every folder.
2. **Run the stages** in the order `mailpipe stages` lists them, each with `--since 1d` (or the time since the last scan). Stages that only make sense on another's labels take `--labelled stage:label` too. For example, a transaction classifier can run on `category:transactional` and an event check on `category:event`. See [stages.md](stages.md).
3. **`mailpipe plan create`**, then read the plan (`plan show <n>`). Look for:
   - a platform or system sender about to be blocked, such as a quarantine or notification service;
   - a label that doesn't fit the email;
   - a sender the owner keeps.

   Fix the policy and create the plan again rather than applying a wrong one.
4. **`mailpipe apply <n>`** (a check), then `--execute` with approval. Its hint gives Graph's before and after counts.
5. **`mailpipe rules diff`**, then `rules apply --execute` with approval, when the plan blocked new senders.
6. **Filing:** run the template stage with `--since`, then `file draft --out <new file>`. Show the owner the draft, then `save … --execute` with approval. Bring the owner `file queue --records tx-kind:record`. See [filing.md](filing.md).

Costs depend on the configured stages, models and message volume. Set explicit cost caps.

## When things go wrong

- **Interrupted:** every command resumes. Rerun the same command.
- **Signed out:** `mailpipe auth login` (a device code to enter in a browser). A command that needs sign-in says so, with exit code 3.
- **A wrong deletion:** the mail is in Deleted Items. `actions <plan>` lists what moved. Deleted Items is purged from time to time, so act soon, and save anything worth keeping before it goes.
- **Blank metadata after a sync:** fixed in the sync. A full pass (`sync --folder inbox --full`) repairs any older blank rows.

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
