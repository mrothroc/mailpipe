# AGENTS.md: mailpipe

mailpipe is a local Go CLI that mirrors a Microsoft 365 mailbox into SQLite, runs messages through pluggable stages, and turns their verdicts into reviewed actions: deleting, blocking senders, and filing records as PDFs. Agents drive it like any Unix tool.

## Start here

- **Commands and flags:** run `mailpipe --help`, then `mailpipe <command> --help`. The CLI documents itself, so these docs don't repeat it.
- **Docs**, read as needed:
  - [docs/development.md](docs/development.md): setup, secret hooks and build security gates
  - [docs/architecture.md](docs/architecture.md): layers, packages, the output contract, where private data lives
  - [docs/hooks.md](docs/hooks.md): private classifier hooks and the subprocess contract
  - [docs/stages.md](docs/stages.md): the stage contract and the stage types
  - [docs/policy.md](docs/policy.md): how verdicts become actions, and the blocklist
  - [docs/filing.md](docs/filing.md): templates, drafting manifests, the owner's queue
  - [docs/operations.md](docs/operations.md): running a scan by hand, and what needs the owner's approval
- **[PLAN.md](PLAN.md):** the supported capabilities, future work and public repository boundary.

## Rules

- **Private data never enters the repository.** The owner's configuration, templates, database and outputs live in `~/mail`. Test fixtures use invented `example.com/org/net` data.
- **Secrets stay in the macOS Keychain.** Never print, log or write the OpenRouter key or the token cache.
- **Anything that changes mail or the server needs the owner's approval for that change:** `apply --execute`, `rules apply --execute` and `save --execute`. See [docs/operations.md](docs/operations.md).
- **Workflow:** test first; `make check` passes before a commit; review the change; use conventional commits.
- **Public fixtures:** never copy personal mail, sender lists, vendors, retention choices or private classifier names into tests or docs. Use invented example domains and generic use cases. Personal classifier implementations live outside this repository.
