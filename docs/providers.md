# Provider setup

Select separate private configuration and database files for each account. Set
`MAILPIPE_CONFIG` and `MAILPIPE_DB` consistently for every command in that session;
set stages, templates and output overrides too when those differ. See the
[README](../README.md) for installation and path variables.

## Microsoft 365

Use the Entra public-client registration and device-code steps in the
[README](../README.md#configure-microsoft-365-access). A fresh empty mirror binds
on its first successful sync to the authenticated immutable mailbox subject.
An existing mirror requires [explicit historical attribution](recovery.md).
`account show` reports configuration, not a fresh authentication or database
verification. Use the command's actual failure to distinguish these states.

## Yahoo read-only setup

Use `mailpipe config init --provider yahoo --login <exact-login>` with separate
private configuration/database paths for each account. `--endpoint` accepts the
[documented implicit-TLS grammar](adr/0006-yahoo-read-only-setup.md); omitting it
uses the provider default. If special-use role evidence is missing, configure
exact existing selectable paths in private `imap.role_mappings` with keys
`sent`, `trash` and `junk`; discovery validates them and creates no folders. Inspect `config init --help`, `account show` and
`account capabilities`. Enter the app password through hidden stdin with
`auth login`; nonterminal secure producers require `--password-stdin`. Never put
a password in argv, environment or a file. `auth status` reports stored credential
readiness, not a fresh remote authentication proof.

Yahoo supports real read-only sync/content, configured stages and local plans.
Native rule installation and all three `apply`, `rules apply` and `save` execution
paths are unavailable, even for empty plans or already-saved records. Scan-time
policy remains local intent and does not install Yahoo filters. Dry save checks
a manifest without authorizing PDF output.

Changing endpoint/login requires a different database; there is no rebind. Failed
credential persistence leaves incomplete readiness: retry `auth login` with fresh
input for the same principal. A routing-save failure retains database binding;
repair only the same principal's configuration. Gmail OAuth and policy integration
are not part of this pilot.

## Processing consent after setup

Fresh setup disables model and trusted subprocess transfers. Existing Graph
configurations without a processing record retain configured legacy behavior;
Yahoo has no such compatibility exception. Explicit records apply only to their
exact account.

For either provider, sync first and inspect `account show` for the exact account
identifier. After the owner authorizes the relevant transfers, use
`config processing --account <id> --models=true` for configured model stages and/or
`--external-stages=true` for trusted exec/hook stages. These are independent scopes;
configuration does not retrieve a model API key or sandbox a subprocess. See
[stages](stages.md) and [hooks](hooks.md).

## Scope

Yahoo's configured principal establishes endpoint/login attribution rather than
an immutable remote subject. Password rotation for the same principal preserves
identity; changing endpoint/login requires another database. Server-side reuse of
the same login cannot be detected as a different owner. Graph's subject proof and
IMAP's configured-principal proof must not be treated as interchangeable.

`account capabilities` reports implemented methods, not permission to execute.
Yahoo local plans are reviewable intent; Microsoft 365 execution examples do not
make Yahoo writable. Gmail is not implemented.
