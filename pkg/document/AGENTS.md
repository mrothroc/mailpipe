# AGENTS.md: MIME content

Read [bounded content](../../docs/adr/0005-bounded-imap-content.md) before changing
parsing, quotas or cleanup, and [attachment extraction](../../docs/adr/0008-attachment-extraction.md)
for snapshot and publication boundaries.

- Use operation-owned streaming scopes; compatibility collectors are not a reason
  to materialize entire messages in production consumers.
- Incomplete input and cleanup failures must remain visible and protective. Never
  shorten content and report a successful classification or filing match.
- Preserve identity, private spool ownership, aggregate accounting and complete
  traversal of ignored MIME parts. Mail-provided names never choose spool paths.
