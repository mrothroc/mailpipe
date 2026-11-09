# Documentation

Start with `mailpipe --help` and the relevant command's `--help` for flags and
output. A checkout's `bin/mailpipe` and the executable on `PATH` can differ.

| Task | Guide |
|---|---|
| Install and get started | [Project README](../README.md) |
| Configure an account and processing consent | [Providers](providers.md) |
| Run and review a scan | [Operations](operations.md) |
| Diagnose binding, migration or interrupted work | [Recovery](recovery.md) |
| Reconcile unavailable historical Graph references | [Reviewed reconciliation](graph-reconciliation.md) |
| Understand layers, identity and output | [Architecture](architecture.md) |
| Configure classifiers and actions | [Stages](stages.md), [policy](policy.md) |
| Maintain a private classifier | [Hooks](hooks.md) |
| File records or inspect MIME files | [Filing](filing.md), [attachments](attachments.md) |
| Develop, test and scan source | [Development](development.md), [CLI development](cli-development.md) |
| Publish source and Homebrew updates | [Releases](releases.md) |
| Check supported scope and future work | [Roadmap](../PLAN.md) |

## Design records

ADRs preserve decisions and their original context. Current operational behavior
is described in the guides above; implementation contracts are in source and tests.

- [Provider-neutral contracts](adr/0001-provider-neutral-mailbox-contracts.md)
- [Account-bound storage](adr/0002-account-bound-storage.md)
- [Graph integration](adr/0003-graph-provider-integration.md)
- [Read-only IMAP](adr/0004-read-only-imap.md)
- [Bounded content](adr/0005-bounded-imap-content.md)
- [Yahoo setup and consent](adr/0006-yahoo-read-only-setup.md)
- [Attachment extraction](adr/0008-attachment-extraction.md)
- [Complete image filing](adr/0009-complete-image-filing.md)
- [Exact-ID historical reconciliation](adr/0010-exact-id-reconciliation.md)
- [Reconciliation approval and audit](adr/0011-reconciliation-approval-audit-digests.md)
