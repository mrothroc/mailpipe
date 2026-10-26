# Filing records

Records can be saved as PDFs into a user-selected filing structure. Configure destinations and document naming rules privately; no particular folder structure or filing preference is required. Templates make each decision once per kind of email, not once per email.

## Templates

A template describes one kind of email from one sender: how to recognise it, which fields to take from it, and, if it is filed, the folder and name it goes under. They live in the private `~/mail/templates.json`, because they name real senders and folders. `examples/templates.example.json` shows the format with invented values, and `pkg/template/template.go` is the reference.

- **The fingerprint is deterministic.** The subject pattern matches, every anchor (a line found in all past examples) appears in the readable body, and every field extracts and validates. A date must parse and lie near the email's date.
- **A filed template's name starts with the document's own date,** when the user chooses date-based naming. The year in a folder comes from that date, not the received date. A January receipt for a December gift belongs to December's tax year.
- **A template without `file`** is a known kind that is not filed. It keeps that sender's routine mail out of the owner's queue.

## The template stage

The stage needs two independent checks to agree before anything is filed automatically: the fingerprint, and Jev choosing among the sender's template descriptions or "none".

| Outcome | When |
|---|---|
| `matched` (with the id, plus `file` if filed) | exactly one fingerprint fits, and Jev chose it at or above the threshold |
| `review` | they disagree, or two fingerprints fit |
| `new` | both say none: a kind this sender has no template for |
| no labels | the sender has no template (no body fetched, no model call) |

A changed receipt layout can cause the fingerprint and model to disagree. Such messages go to review instead of being filed silently.

## The filing commands

- **`file draft --out <new file>`** writes a manifest for `save`: one row per matched, filed email not yet saved. It records each row as `filing:pending`, so the record shelf life spares it until it is saved. It never replaces a file.
- **`save --manifest <file> --root ~/Documents`** checks the manifest, and with `--execute` writes the PDFs:
  - an attachment is saved as is;
  - an email without one is rendered offline;
  - several attachments get names ending ` - <attachment name>`.

  Files are created with mode 0600 and never overwritten, and a rerun recognises what is already saved.
- **`file queue --records tx-kind:record`** lists what waits for the owner: reviews, new kinds, and records from senders with no template. It covers the Inbox only, and leaves out saved and dismissed emails. (`tx-kind:record` is an example; use whichever stage and label mark records.)
- **`file dismiss <n>... --reason <why>`** records the owner's decision not to file. It goes to the `filing` stage, beside `pending` and `saved`, so rerunning the template stage cannot bring the email back to the queue. Decisions first recorded as template verdicts were undone by exactly such a rerun.
- **`templates check [--expect <manifest>]`** runs the fingerprint for free over stored mail. With `--expect`, it compares the result with an earlier manifest.
- **`templates anchors --from <sender> [--subject <regexp>]`** lists the lines common to all of a sender's examples, a starting point for anchors.

## Writing templates

- **Templates are written by an agent from real examples and approved by the owner.** No model writes or changes one on its own.
- **Test a new or changed template** with `templates check` (and `--expect` against what the owner filed before), then `run template --rerun --folder= --from <sender>`.
- **Route by content when one sender serves several recipients.** One template per destination, with a distinguishing field as an anchor, keeps records apart.
- **Descriptions say what they exclude.** Distinguish adjacent kinds, such as invoices and payment reminders, to reduce ambiguous matches.
- **Describe what the subject shows when the body is empty.** Emails that are only an attachment give Jev little to read, and its confidence stays below the threshold.
- **The stage runs over all mail, not only records.** A record classifier may miss records, so template matching should run independently.
