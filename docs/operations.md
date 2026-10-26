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
