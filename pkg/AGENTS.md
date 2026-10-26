# AGENTS.md: pkg

The reusable layers, kept free of the CLI so a scheduled or Cloud Run worker can use them later. The packages and their roles are listed in [docs/architecture.md](../docs/architecture.md).

- **No cobra, no printing, no exit codes here.** Return errors and data; `internal/mailpipe/cli` presents them. The one agentio use is `RegisterSecret`, which keeps tokens out of output.
- **Each package's doc comment is its contract,** and generates the package table in the docs. When a package's role changes, update its comment, then run `make docs`. `make check` fails while they disagree.
- **Stages judge, policy decides, apply acts** (`stage`, `policy`, `apply`). Keep that split. No stage moves mail, and nothing moves mail outside a stored plan.
- **Inputs from mail are untrusted.** Parse defensively, render offline, and never pass message text to a shell.
