# CLI development

The CLI composes providers and reusable packages. Keep policy, protocol and storage
logic in their owning packages; see [architecture](architecture.md).

Use `newAgentCommand` for leaves and `newGroup` for groups (`agentcmd.go`). The
wrappers provide the shared agentio flags, error presentation and footer. Use
`agentio.Tabled` for TSV/JSON from one row type; hints go to stderr. Return typed
agentio errors with an actionable next command, and use `classify` for shared
errors. A generic timeout does not establish that an account is signed out or a
database is unbound.

Inject collaborators through `Deps`. Reuse `openStore`, `lazyProvider`,
`messageBody` and `remoteRef` rather than opening a second provider/store path.
`lazyProvider` returns an owned cleanup function; join work and surface cleanup
failures before reporting success. Graph-only operations use the existing Graph
seams and capability checks. IMAP whole-pass sync is distinct from the paged Graph
interface; do not fabricate unsupported provider methods.

Reuse `stageLabelFlag` and `parseSince`. Read the surrounding command's tests for
the account-bound and offline paths before changing its collaborators.

## Effects and tests

Do not apply a blanket dry-run rule to every local write: sync, verdict recording,
plan creation and filing drafts intentionally persist local state. Mailbox/rule
execution and PDF saving retain their explicit checks and owner approval. Attachment
extraction separately checks selected local writes before `--execute`. Output
writers preserve private permissions, containment and no replacement of existing
files; use the established writers rather than inventing path handling.

Tests drive the real command tree through `env.run` in `cli_test.go`, using fake
collaborators, temporary stores and invented configuration. `TestMain` isolates
overflow output. Include meaningful provider capability, stale identity and
offline cases when changing these paths; a fake Graph alone cannot establish
Yahoo behavior. Real renderer verification is described in
[development](development.md).
