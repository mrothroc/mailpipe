# Architecture

mailpipe is one Go module organised into `cmd/`, `internal/` and `pkg/`, so the reusable layers in `pkg/` can later back a scheduled or Cloud Run worker without the CLI.

## Layers

Each layer only hands data to the next, so a new source, stage or action plugs in without touching the others.

1. **Source.** Microsoft Graph, through delegated device-code sign-in (a public client, no secret). A Graph mailbox adapter converts wire data into account-bound domain references. Protocol scanners supply complete pages; the shared sync lifecycle and account store publish them atomically. Bulk changes use reviewed identity checks.
2. **Store.** A permanently account/provider-bound SQLite mirror of metadata and memberships, with bodies cached on demand, verdict provenance, plans and an action log. Stable local numbers (`n`) survive ordinary moves. Historical attribution is explicit; unbound and account-aware APIs remain separate.
3. **Stages.** Stages judge messages and return verdicts. They never act on mail. See [stages.md](stages.md).
4. **Policy.** It turns the latest verdict of each stage into an action: keep, quarantine, delete or digest. It lives in private configuration, apart from the stages. See [policy.md](policy.md).
5. **Actions.**
   - **Plans:** an action happens only through a stored, numbered plan. `apply` moves its messages with Graph JSON batching, re-checking account, immutable identity/locator, Message-ID, sender, membership and any revision immediately before execution, and logs attempts with returned state.
   - **Blocklist:** server-side Inbox rules stop new mail from blocked senders.
   - **Filing:** records are saved as PDFs. See [filing.md](filing.md).

## Packages

The table is generated from each package's doc comment (`tools/docgen`). Keep the doc comments accurate, and run `make docs`.

<!-- docgen:packages -->
| Package | What it does |
|---|---|
| `cmd/mailpipe` | Command mailpipe is the mailpipe CLI entry point. |
| `internal/mailpipe/cli` | Package cli implements the mailpipe command line. |
| `internal/mailpipe/config` | Package config reads mailpipe's private per-user configuration: the Entra app registration it signs in through (~/mail/config.json) and the stages, policy and owner's addresses (~/mail/stages.json), plus verified account and collection routing. |
| `pkg/apply` | Package apply carries out a stored plan against the mailbox. |
| `pkg/document` | Package document reads an email's MIME source into the parts a filing step needs: its headers, text and HTML bodies, inline images and attachments, and an offline HTML page for rendering it (inline images embedded, remote resources removed, so rendering never fetches anything). |
| `pkg/graph` | Package graph is a thin Microsoft Graph client: bearer auth, immutable message ids (stable across folder moves), text bodies, throttling retries and paging. |
| `pkg/graphmail` | Package graphmail adapts Microsoft Graph mailbox data and reviewed operations to the provider-neutral mailbox contract. |
| `pkg/keychain` | Package keychain stores secrets in the macOS login Keychain through the security tool. |
| `pkg/mailbox` | Package mailbox defines provider-neutral accounts, message identities, collection memberships and optional reviewed capabilities. |
| `pkg/mailsync` | Package mailsync coordinates account-bound sync passes. |
| `pkg/msauth` | Package msauth signs in to Microsoft 365 with the device code flow (a public client, no secret) and keeps MSAL's token cache in the macOS Keychain. |
| `pkg/policy` | Package policy turns stage verdicts into actions. |
| `pkg/render` | Package render turns an HTML page into a PDF with headless Chrome. |
| `pkg/rules` | Package rules turns what applied plans deleted into one managed blocklist of server-side Inbox rules, so future mail from those senders goes straight to Deleted Items. |
| `pkg/stage` | Package stage defines the contract between mailpipe and the stages that judge messages. |
| `pkg/store` | Package store persists mailbox metadata, account-bound identities, memberships and reviewed action history in private SQLite databases. |
| `pkg/template` | Package template recognises the kinds of email a sender sends, so a record can be filed by rule instead of by hand. |
| `tools/docgen` | Command docgen generates and checks source-derived package documentation. |
| `tools/security` | Command security checks public files for secrets and verifies Go release support. |
<!-- /docgen:packages -->

## The output contract

Every command follows the shared agent CLI contract in `github.com/mrothroc/agentio` (version pinned in `go.mod`):
- stdout carries only data: TSV with a header row, or JSON Lines with `--json`;
- stderr carries hints, `[error] <command>: <what>. Try: <command>` and a one-line footer;
- exit codes are fixed: 0 ok, 1 runtime, 2 usage, 3 stale (signed out or unconfigured), 4 no data;
- `--limit` bounds stdout, and the rest overflows to a private file under `~/mail/out`, named in a hint.

How commands are built on it is in [internal/mailpipe/cli/AGENTS.md](../internal/mailpipe/cli/AGENTS.md).

## Where private data lives

Everything personal lives outside the repository: configuration, templates naming real senders, the database, plans and outputs. It sits in `~/mail` (directory 0700, files 0600). Each path can be moved with a `MAILPIPE_*` environment variable, read in `internal/mailpipe/config` and the CLI; tests use them to stay out of the real data. Tokens and the OpenRouter key live in the macOS Keychain; Keychain writes go through `security -i` on stdin, so nothing appears in `ps`.

## Security

- **Least privilege:** delegated access to the signed-in user's own mailbox only.
- **Recoverable destinations:** destructive commands are dry runs without `--execute`, and they move mail to Deleted Items (recoverable) rather than deleting it.
- **Untrusted mail content:** mail bodies are data. Model scores can be manipulated; they are not execution authority. The built-in model stage maps scores only to configured labels, and actions use the identities in a stored plan. A poisoned verdict can still affect later policy decisions, sender caches or blocking rules, so review those effects before execution. External stages run as subprocesses, with no shell interpolation.
- **CLI display:** terminal controls and bidirectional formatting controls are displayed as visible escapes on stdout and stderr. Normal Unicode, tabs and line breaks remain readable; JSON strings retain their original decoded values. Raw mail and overflow files remain untrusted data.
- **Authenticated requests:** initial Graph URLs and every redirect must use the configured API origin, without URL user information or fragments. Cross-origin pagination and redirects fail before credentials can reach another origin.
- **Filing containment:** executing `save` opens the chosen filing root once. Existence checks and file creation use rooted filesystem operations, with a pinned parent directory for temporary files, linking and cleanup. Relative symlinks contained within the root are allowed; escaping or absolute symlinks are rejected. Files retain mode 0600 and existing files are never replaced.
- **Offline rendering:** the PDF renderer parses email HTML into an allowlist of static presentation elements and attributes, then places a restrictive Content Security Policy before that content. Scripts, embedded documents and local or remote resource loads are blocked; inline raster images and CSS remain available. A throwaway browser profile and dead proxy provide additional isolation.

### Prompt injection regression tests

`pkg/stage/injection_test.go` and `pkg/apply/injection_test.go` exercise instruction overrides, role spoofing, request-shaped text, hostile model response fields, and attempts to expand or execute a stored plan. Run them with `go test ./pkg/stage ./pkg/apply -run 'Injection|Compromised|Hostile'`.

The fixtures are invented, use reserved example domains, and contain no mailbox excerpts, owner decisions or private policy settings. Tests use a local mock model endpoint, a temporary database and a fake mailbox; they require no credentials or live services. The mock deliberately supplies an attacker-favourable score to check containment even when classification fails. Passing these tests does not establish a live model's resistance to injection, or the safety of an agent reading message text through CLI output. Such text remains untrusted and cannot grant approval.
