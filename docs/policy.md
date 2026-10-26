# Policy

The policy turns each message's latest verdict from every stage into one action: keep, quarantine, delete or digest. It lives in the `policy` section of the private `~/mail/stages.json`, because it names real senders. `pkg/policy/policy.go` is the reference for every field; this page explains how they combine and why.

## How a decision is made

- **The most protective action wins.** A keep beats a quarantine, which beats a delete. Every reason for the winning action is recorded, so a plan shows why.
- **`keep_senders`** are never acted on, whatever any stage says.
- **Rules** match a stage's label (`stage` + `label`) or a sender (`senders`: addresses, or `@domain`), and name an action. Options:
  - `older_than` ("7d", "36h"): the rule applies only to mail at least that old. Durations are chosen by the user; there are no built-in retention periods.
  - `unless` (a list of `stage:label`): skip the rule for mail carrying any of those labels, for example records that are waiting to be filed.
  - `no_block`: the deletion never blocks the sender. Use it for mail that simply expires, from senders whose new mail is wanted.
  - `hold` (with `senders` and `older_than`): sets that sender's lifetime. Their mail is kept until it is that old, whatever other rules say, then the rule applies even over keep rules. Only the allowlist still wins. This allows a private sender-specific lifetime to override general label rules.
- **`block_senders`:** all their mail is deleted and the sender is blocked. A keep still wins.
- **`junk_senders`:** a server rule moves their new mail to Junk. They are never blocked.
- **`never_block`:** single messages may be deleted, but the sender never joins the blocklist. Use it for platforms (such as shared notification services) that also send mail worth having.

## Plans

`mailpipe plan create` records the policy's decision for every live message as a numbered plan. Nothing changes until `apply <plan> --execute`, and the owner reviews the plan first (see [operations.md](operations.md)).

## The blocklist

A deletion blocks its sender unless every reason for it is `no_block`. `rules` turns the senders of applied deletions into one managed blocklist of server-side Inbox rules, so their new mail goes straight to Deleted Items.
- **A sender is left off** when any of their mail is kept anywhere in the mailbox, when their mail only expired, or when they are protected: a keep sender, `never_block`, a junk sender, the owner, or a contact. Automated senders stay protected here, though the allowlist doesn't keep their mail.
- **`rules diff`** shows the changes; `rules apply --execute` writes them and reads them back.
- Only rules mailpipe wrote are touched (`mailpipe blocked #k`, `mailpipe junk #k`). The owner's own rules are never changed.

## Design principles

- Expiry does not imply that future mail from a sender is unwanted. Use `no_block` for retention rules.
- A message label does not establish the reputation of its sender. Shared services may quote unrelated content; protect their senders when appropriate.
- Retention periods, sender lists and classifications belong in private user configuration, not in application defaults.
