# AGENTS.md: the mailpipe CLI

The cobra commands, built on the agentio contract (see [docs/architecture.md](../../../docs/architecture.md)). Domain logic belongs in `pkg/`; this package parses flags, wires collaborators, and prints.

## Conventions for a new command

- **Build leaves with `newAgentCommand`, and groups with `newGroup` or a parent leaf** (`agentcmd.go`). They give usage errors, the `[error] … Try:` line, `--json`, `--limit` and the footer for free.
- **Return agentio errors with a next step.** Use `NewUsageError` (2), `NewError` (1), `NewStaleError` (3) or `NewDataError` (4), each with a `Try:` command. `classify` (`deps.go`) maps sign-in, configuration and interruption errors.
- **Print data with `agentio.Tabled`** (TSV and JSON from one row type). Hints go to stderr as `[hint] …`, ending with the command to run next.
- **Collaborators come from `Deps`** (`deps.go`), and tests replace every field. Use `lazyGraph` so a command that may not need Graph never prompts for sign-in. Use `messageBody` for a cached-or-fetched body.
- **Shared parsers:** `stageLabelFlag` reads `stage:label` flags (`stages.go`), and `parseSince` reads `--since` (`since.go`). Don't write another.
- **Anything that changes mail, the server or files** is a dry run unless `--execute` is given. A file it writes is created with mode 0600 and never replaces an existing one.

## Tests

Tests drive the real command tree through `env.run` (`cli_test.go`), against a fake Graph, a temporary database, and stage and template configuration written by the test. `TestMain` points the overflow directory away from the real `~/mail/out`. Fixtures use invented `example.*` data.
