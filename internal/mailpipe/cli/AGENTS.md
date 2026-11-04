# AGENTS.md: CLI

Parse flags, wire collaborators and present results here; reusable domain logic
belongs in `pkg/`. Read [CLI development](../../../docs/cli-development.md) before
adding commands, and [architecture](../../../docs/architecture.md) for output and
provider contracts.

- Use the shared command wrappers, parsers and injected `Deps`.
- Keep provider access lazy and close owned clients; cached/offline paths must not
  authenticate unnecessarily. Do not assume every provider implements mutations.
- Preserve explicit execution checks and account/identity evidence. Local state
  recording is distinct from mailbox execution and reviewed PDF saving.
- Tests drive the real command tree with invented fixtures and isolated private
  paths. Never use the owner's configuration or mailbox in automated tests.
