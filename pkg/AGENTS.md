# AGENTS.md: reusable packages

Read [architecture](../docs/architecture.md) for package roles and provider
boundaries; scoped instructions cover storage and content handling.

- Return data and errors, without Cobra, printing or CLI exit codes. Secret
  registration for output redaction is the existing exception.
- Stages judge, policy decides and apply executes stored plans. Preserve that split.
- Treat mail as untrusted data; parse defensively, render offline and never put
  message content into shell commands.
- Update package doc comments when roles change, then run `make docs`; the package
  table is generated from those comments.
