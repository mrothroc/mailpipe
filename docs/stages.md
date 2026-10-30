# Stages

A stage judges one message and returns a verdict. It never acts on mail: [policy](policy.md) decides what verdicts lead to. Model output is untrusted: labels can affect downstream policy and sender-level decisions, so review plans and rules before execution.

## The contract

A stage receives a message as JSON: metadata, plus the readable body when the stage needs it. It returns a verdict: labels, scores, an optional suggested action, a reason, its cost, and its provenance. The fields and their validation are in `pkg/stage/stage.go`. Built-in and external stages share one contract and one set of contract tests.

An **external stage** is any executable. It reads the message on stdin, writes the verdict on stdout, and uses stderr and its exit code as Unix does. `examples/stages/newsletter.sh` is a working example.

## Stage types

Stages are defined in the private `~/mail/stages.json`; `examples/stages.example.json` shows a starting configuration with invented values. Optional stages must be added explicitly. Defaults (thresholds, models, timeouts) are set in `internal/mailpipe/config/stages.go`.

- **allowlist:** keeps mail from the owner's own addresses and domains, and from anyone the owner has written to (Sent Items). A hit is final.
  - `automated_senders` are never kept, even on the owner's domains. These are agents or scripts whose mail should age out like anyone else's.
- **honeypot (optional):** for users who designate a recipient address for filtering. Add a `honeypot` stage and a private `honeypots` list to enable it. The shipped example does not enable this feature; an omitted or empty address list produces no labels.
- **jev:** asks the Jev decision model, through OpenRouter's Decisions API, one or more yes/no questions in a single call. A score at or above the threshold adds the question's key as a label. `{today}` in a question becomes the date, for questions that change with time ("has this event passed?").
- **sender_cache:** reuses another stage's verdicts on a sender's earlier mail, so a sender with a settled history needs no new model call.
- **exec:** an external stage, as above.
- **hook:** a private classifier executable with optional MIME access, exact sender scoping and a shared cost allowance. See [hooks.md](hooks.md).
- **template:** recognises a record's kind and where to file it. See [filing.md](filing.md).

## Running stages

- `mailpipe run <stage>` judges only messages that stage hasn't judged yet; `--rerun` judges again.
- **Give every paid stage `--since`.** Without it, a stage judges the whole backlog it hasn't seen.
- **Chain stages with `--labelled stage:label`.** For example, run a transaction classifier only on what the category stage labelled transactional.
- **The latest verdict of each stage counts.** Rerunning a stage supersedes its earlier verdicts. So an owner's decision is never recorded as a stage verdict that a rerun could overwrite; filing decisions go to the `filing` stage (see [filing.md](filing.md)).
- **Paid stages stop at `--max-cost-usd`,** checked before each call. A typical Jev call costs a few thousandths of a cent, so a cap of a few cents covers a day's mail.
- **Bodies are fetched only when a stage needs them,** and are cached in the store. A stage can choose per message (`stage.BodyChooser`). The template stage fetches none for senders without a template.

## Model input and conservative decisions

Jev and template stages send the complete readable message, including headers and dates at the end. URL noise is removed; message text is never silently truncated. `max_input_bytes` on either stage sets the maximum readable message size, including headers. The default is 1 MiB (1,048,576 bytes), not a token count or a guarantee that every model accepts that input. Configure a lower limit for smaller models or a higher limit when appropriate.

An input over the limit makes no model call. The stage records a `review` label and a `keep` action, so other deletion rules cannot turn partial evidence into a deletion. Template inputs over the limit also appear in the filing review queue and cannot produce a filing match. Inspect or split exceptionally long messages before rerunning them. Normal model API errors remain errors; they are never silently retried with shortened text.

Each Jev question can set its own `threshold`, overriding the stage threshold. An omitted or zero question threshold inherits the stage setting. For destructive classifications, require strong evidence for the deletion label and use a lower threshold for a protective label. A policy rule's `unless` must reference that protective label, so conflicting results retain the message.

For date-based expiry, ask whether **all** relevant events, deadlines and action items are over. A newsletter may mix completed events with future registrations or contain unclear dates. Any upcoming or ongoing item should prevent expiry, and missing or ambiguous dates should not establish that a message is expired. Use a separate uncertainty label as an additional `unless` guard on both event-completion rules and age-based event expiry. Test borderline scores, conflicting answers, mixed dates, and long messages with important information near the end using invented fixtures. Mock-model tests verify the application boundaries; they do not establish a live model's date reasoning accuracy.
