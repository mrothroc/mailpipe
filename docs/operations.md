# Operations

How to run a scan by hand, and which steps need the owner. Scans run on demand; scheduling remains future work ([PLAN.md](../PLAN.md)).
For account setup read [providers](providers.md); for binding, migration or failures
read [recovery](recovery.md). The execution workflow below is for Microsoft 365.
Yahoo supports sync, classification and local review, with the capability limits
described in the setup guide.

## What needs the owner's approval

Read-only and recording steps can run freely. Each of these changes mail, the server or the owner's files, and needs the owner's approval for that specific change:

| Command | Effect |
|---|---|
| `apply <plan> --execute` | moves the plan's messages to Deleted Items |
| `rules apply --execute` | rewrites the managed blocklist rules on the mail server |
| `save --manifest <file> --execute` | writes PDFs into `~/Documents` |
| changes to `~/mail/stages.json` or `~/mail/templates.json` | change what future scans do; back the file up first |

Without `--execute`, the execution commands check the proposed operation without
performing the listed move, server-rule rewrite or PDF save. Run the check first
and show the owner the result. Local opening, migration and caching are separate
effects; read [recovery](recovery.md) before opening an older mirror. Private
configuration edits have no implicit dry-run mode.

## A manual scan

1. **Choose the exact context.** Record the executable, configuration/database
   paths and account privately. Inspect `account show` and the relevant help.
   Use this same context throughout review and execution. A temporary mirror's
   message and plan numbers cannot be used against the normal database.
2. **`mailpipe sync`** updates the mirror. Verify per-folder local/server counts
   and successful completion; investigate mismatches rather than declaring a full
   scan from an interrupted or limited pass.
3. **Run configured stages in dependency order**, using `stages` and the private
   configuration. Cover mail since the last successfully completed scan rather
   than assuming a fixed daily window. Use `--labelled stage:label` where a stage
   depends on earlier labels, and explicit cost caps for paid calls. Old messages
   with time-sensitive verdicts may need `--rerun` even without new mail.
   See [stages](stages.md).
4. **Review filing before the final deletion plan.** Run the configured template
   stage, `file draft --out <new file>` and the owner queue. Drafting records
   `filing:pending`; policy must protect records awaiting filing. Exit 4 with
   “nothing to draft” is an expected empty outcome, not a failed save or proof
   that the review queue is empty. See [filing](filing.md).
5. **`mailpipe plan create`**, then inspect `plan show <n>`. Check each deletion's
   labels, all matching retention rules, protected records, future/uncertain
   events and sender blocking implications. Correct the private policy only with
   authorization and create a fresh plan when findings change its decisions.
   An unrelated age rule can still delete an upcoming event unless it has its
   own protective guard; see [policy](policy.md).
6. **`mailpipe apply <n>`** checks the exact plan. Show the reviewed result and
   execute with `--execute` only when that change is approved. The tool performs
   the moves and audit; do not reproduce its operations with ad hoc Graph calls.
7. **`mailpipe rules diff`** reviews managed server-rule changes after applicable
   executed deletions. `rules apply --execute` needs approval for that diff.
   Use the canonical action history, not an isolated scratch mirror.
8. Keep the scan interval, costs, review findings, manifest/plan identifiers and
   outcomes in private output. Distinguish new filing candidates from an older
   unresolved queue and report anything left for the owner.

Costs depend on the configured stages, models and message volume. Set explicit cost caps.

## Filing a single record by hand

For one message, templates are optional. `mailpipe attachments list <n>` shows its
true MIME files, and `mailpipe attachments extract <n> --part <selector> --out-dir
<private dir>` (check first, then `--execute` to authorize that local write) writes
the selected decoded bytes. Raw extraction does not mark the message saved and does
not authorize deletion. Then write a manifest (`n<TAB>folder<TAB>name`), check it
with `mailpipe save --manifest <file> --root <dir>`, and execute only after the
owner approves that specific save. Neither step mutates server mail or deletes it:
extraction writes local files, and `save` creates PDFs and records a local
`filing:saved` verdict. Deleting mail needs a separately reviewed plan and its own
approval. This is the same manifest `file draft` writes from templates. See
[attachments.md](attachments.md) and [filing.md](filing.md).

## Recovery

Do not infer account state from a timeout or blindly rerun uncertain execution.
Read [recovery](recovery.md) for migration, historical attribution, locks, partial
effects and safe isolated surveys.
