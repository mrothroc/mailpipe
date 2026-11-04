# AGENTS.md: documentation

Use [the index](README.md) to find the focused guide. Keep agent files short and
link detailed instructions; avoid duplicating command flags or configuration tables.

- Verify current behavior against CLI help, source and tests. ADR context describes
  the decision at its time; preserve it and link later decisions when needed.
- Source-derived values belong in generated regions or links to their authoritative
  source. Do not hand-edit the generated package table; run `make docs-check`.
- Keep examples invented and generic. No personal configuration, live scan reports,
  private policy choices, mailbox identifiers or credentials belong here.
- Check relative links and the diff for identifying prose as well as secrets.
