# mailpipe

I was tired of handling a ludicrous amount of email, so I built mailpipe. It is designed as a CLI to be used by an agent to let it help you classify and manage email in a Microsoft 365 mailbox in bulk. I originally tried just controlling the Mail app on my Mac, but it was way too slow.

I use it to automatically delete spam and block senders. It also saves records (like receipts) as PDFs to the local disk, then deletes the emails. Finally, for certain categories, it will automatically delete messages that have been sitting in my inbox for a configured time period so things don't pile up.

Theoretically a human can use it, but it is intended to be used by an agent.

It mirrors messages into SQLite, runs configurable classifiers, and turns their labels into reviewed plans for moving mail to Deleted Items, managing sender rules, or filing records as PDFs.

Classifiers judge messages; policy and numbered plans control actions. Mailbox changes require an explicit `--execute`. Scans run on demand; no scheduler is installed.

## Requirements

- macOS, with access to the login Keychain. The current authentication implementation uses the macOS `security` command.
- Homebrew for the recommended installation. Source builds require Git, Make and a supported Go release meeting the minimum in [go.mod](go.mod).
- A Microsoft 365 mailbox and a Microsoft Entra app registration for device-code sign-in.
- Network access for Microsoft Graph and any configured model provider. Source builds and security checks also fetch Go modules, release metadata and vulnerability data.
- Optional: an OpenRouter account and API key for model stages. These stages send selected email content to OpenRouter and incur charges.
- Optional: Google Chrome, Chromium or Microsoft Edge installed in `/Applications` for rendering email bodies to PDFs.

## Install with Homebrew

```sh
brew install mrothroc/tap/mailpipe
mailpipe --help
mailpipe_examples="$(brew --prefix mrothroc/tap/mailpipe)/share/mailpipe/examples"
```

The formula builds the tagged source release with Homebrew's Go compiler and installs the executable on `PATH`. Homebrew manages the build dependency; users do not need to install the developer security tools themselves. Public configuration examples and the sample classifier are installed under `share/mailpipe/examples`. Installation and upgrades leave private configuration, mail and credentials alone.

Upgrade with `brew upgrade mrothroc/tap/mailpipe`.

## Build and development setup

```sh
git clone https://github.com/mrothroc/mailpipe.git
cd mailpipe
make install-security-tools
make install-hooks
make check
export PATH="$PWD/bin:$PATH"
mailpipe --help
mailpipe_examples="$PWD/examples"
```

The binary is written to `bin/mailpipe`. `make build` also runs the security gate. Gitleaks scans public source files, `govulncheck` checks dependency modules, and the build verifies module checksums and Go release support. These checks fail when required tools or online security metadata are unavailable. The installed pre-commit hook scans staged content and redacts findings.

The build and documentation checks use Go, Git, Make and the security tools above; no external workflow service is required. See [development.md](docs/development.md) for individual check targets and hook installation details.

## Configure Microsoft 365 access

1. Register an application in your Microsoft Entra tenant. Record its **Application (client) ID** and **Directory (tenant) ID**.
2. Enable **Allow public client flows** under the application's authentication settings. Device-code authentication uses a public client; do not create a client secret for mailpipe. See Microsoft's [authentication flow documentation](https://learn.microsoft.com/en-us/entra/identity-platform/msal-authentication-flows).
3. Add Microsoft Graph **delegated** permissions `Mail.ReadWrite` and `MailboxSettings.ReadWrite`. The first permits mailbox operations; the second permits managing Inbox rules. Your tenant may require administrator consent or restrict device-code sign-in. See the [Graph permissions reference](https://learn.microsoft.com/en-us/graph/permissions-reference).
4. Initialize the local configuration, replacing the placeholders with your own UUIDs:

```sh
mailpipe config init --client-id <application-uuid> --tenant-id <directory-uuid>
mailpipe auth login
mailpipe auth status
```

Follow the device-code instructions printed by `auth login` in a browser. Authentication tokens are stored in the macOS Keychain, rather than the repository or configuration files.

## Configure classifiers and policy

```sh
mkdir -p ~/mail
chmod 700 ~/mail
cp "$mailpipe_examples/stages.example.json" ~/mail/stages.json
cp "$mailpipe_examples/templates.example.json" ~/mail/templates.json
chmod 600 ~/mail/stages.json ~/mail/templates.json
```

These are illustrative starting points. Edit your private copies: replace the example self-addresses and domains, choose which stages to enable, and decide which labels should lead to keeping, deleting or filing mail. There are no built-in sender lists or retention periods. The sample external stage uses `examples/stages/newsletter.sh` relative to the source checkout. In your private stages file, replace it with the absolute path `$mailpipe_examples/stages/newsletter.sh` (expand the variable when editing; configuration does not expand shell variables). You can also disable that sample stage or replace it with your own classifier.

For model stages, create a generic-password item in Keychain Access with service `mailpipe-openrouter`, account `default`, and your OpenRouter API key as the password. These names can be overridden in private configuration. Avoid putting the key in command arguments, source files, environment files or shell history. Metadata-only stages and private local classifiers do not require an OpenRouter key.

Optional recipient filtering is documented in [stages.md](docs/stages.md). Custom classifier implementations, sender scopes, prompts and thresholds belong outside this repository; see [private hooks](docs/hooks.md).

## Run a reviewed cleanup

```sh
mailpipe sync
mailpipe stages
mailpipe run known --since 1d
mailpipe run newsletter --since 1d
```

Run each additional stage you enabled, applying any label filters its input needs. For example, the sample model stage can be run explicitly with a small cost allowance:

```sh
mailpipe run political --since 1d --max-cost-usd 0.05
mailpipe plan create
mailpipe plan show <plan-id>
mailpipe apply <plan-id>
```

The last command checks the proposed moves without performing them. After reviewing every item, execute the approved plan with `mailpipe apply <plan-id> --execute`. Inspect `rules diff` before using `rules apply --execute` to update server rules. Moving mail to Deleted Items is recoverable only while the mailbox retains it.

Use a time window covering mail since the last scan, and explicit cost caps for paid stages. Review classifications and plans: models can make mistakes or be influenced by malicious message content. Classifiers and hook output cannot authorize execution.

The complete workflow, including filing and recovery, is in [operations.md](docs/operations.md). Commands and flags are documented by `mailpipe --help` and `mailpipe <command> --help`.

## Data and customization

Configuration, templates, the SQLite mirror and generated outputs default to `~/mail`, outside the checkout, with private file permissions. `MAILPIPE_CONFIG`, `MAILPIPE_STAGES`, `MAILPIPE_TEMPLATES`, `MAILPIPE_DB` and `MAILPIPE_OUT_DIR` can select other locations. Tokens and API keys remain in the Keychain. Private hooks run as trusted user processes and are not sandboxed.

[Architecture](docs/architecture.md) describes storage and output contracts. [Policy](docs/policy.md), [stages](docs/stages.md) and [filing](docs/filing.md) describe the extension points. Public tests use invented data and do not need access to a real mailbox or model account.

## License

[MIT](LICENSE). Dependencies retain their respective licenses.
