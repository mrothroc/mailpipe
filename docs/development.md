# Development and security checks

Run `make install-security-tools` to install the pinned Gitleaks release into the ignored `bin/` directory, then `make install-hooks` to activate this checkout's tracked Git hooks. Re-run hook installation after a fresh clone or history reset: Git deliberately does not activate repository hooks automatically. An existing custom hook path is not replaced.

The native pre-commit hook runs Gitleaks on the **staged diff**, with secret values redacted. An unstaged edit cannot hide a secret that remains staged. A missing scanner or a finding blocks the commit. This uses Gitleaks directly; the Python `pre-commit` framework is not required.

## Build gate

Both `make build` and `make check` include `make security`:

| Target | Check |
|---|---|
| `make secrets` | Gitleaks scans a temporary snapshot of Git-tracked files plus untracked, nonignored files. Ignored private runtime data is excluded, and symlinks are not followed. |
| `make go-version-check` | Fetches the official Go release feed; checks that the effective compiler is on one of the two supported release families and is its latest published patch. Also checks that the minimum Go family in `go.mod` is still supported. |
| `make modules-check` | `go mod verify` checks downloaded module contents against recorded checksums. |
| `make vuln-check` | A pinned `govulncheck` scans the module graph, including transitive dependencies and the standard library, against the Go vulnerability database. Known affected modules fail the gate even if vulnerable code appears unreachable. Reports identify available fixed versions. |

The Go version and vulnerability checks require network access. Unavailable or malformed release/database responses fail the checks; they are not treated as clean scans. The supported-version rule follows the [Go security policy](https://go.dev/doc/security/policy), using [official release metadata](https://go.dev/dl/?mode=json). The `go` directive is a minimum language/toolchain requirement, so its patch need not equal the latest patch; the compiler actually used for the build must be current.

Tool versions are pinned in `Makefile`. The pinned Gitleaks CLI uses the `git --pre-commit --staged` and `dir` commands; the pinned govulncheck supports `-C` and `-scan=module`. See the [Gitleaks CLI reference](https://github.com/gitleaks/gitleaks#usage) and [govulncheck reference](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck). The vulnerability scanner is invoked through `go run` at that version, independently of any globally installed scanner. Tool module downloads are checked by Go's module authentication mechanisms. When an advisory has a fix, update the affected dependency or compiler and rerun the complete checks. The gate also reports known advisories without a fix; absence of a CVE identifier or apparent unreachability does not suppress them.

`govulncheck` module scanning runs from `cmd/mailpipe` because the repository root contains no Go files. It still checks the enclosing module's dependency graph. For call-path analysis when investigating a finding, run the scanner at symbol level from the module root with `./...`.

## GitHub Actions

The [CI workflow](../.github/workflows/ci.yml) is authoritative for triggers, schedule,
runner and dependencies. It runs the release gate, installs PDF inspection tools
and checks the browser before testing. `make test` and `make check` require rendering
verification: missing or unusable browser/PDF inspection dependencies fail with
installation guidance instead of silently skipping required coverage.

CI uses the latest stable Go compiler; the release gate still checks that both that compiler and the module's minimum Go family are supported. Newly disclosed dependency vulnerabilities or an unsupported minimum Go version therefore fail the weekly run even without a source change. Scheduled runs use the default branch, can be delayed, and GitHub automatically disables schedules in public repositories after 60 days without repository activity; re-enable the workflow in Actions if that happens. See [GitHub's schedule documentation](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule).

The workflow has read-only repository permissions, SHA-pinned official actions, no persisted checkout credentials, and no dependency caches or uploaded artifacts. Fork contributions use `pull_request`, with GitHub's normal approval rules. CI needs no mailbox credentials, model API keys or private configuration; all fixtures are invented. These jobs build and test the project and never operate a real mailbox. Logs are public. The example-address privacy guard also covers workflow YAML under `.github`.

## Publication boundary

The renderer security tests use invented HTML and temporary canary files. Release verification runs actual headless Chrome and Poppler to check script/local-file isolation and complete inline-image filing. Synthetic portrait, landscape and multiple-image PDFs are rasterized with `pdftoppm`; assertions check every image edge, aspect ratio, printable bounds, pagination and visible source headers. Negative dependency tests prove required verification fails when tools are absent. The older standalone renderer tests remain optional outside required mode; the complete-image filing tests require their dependencies in every run. Install Google Chrome and Poppler (`brew install poppler`) before running them. Browser flags alone do not enforce the rendering boundary: parsed HTML sanitization and an early Content Security Policy do.

The attachment acceptance test drives the one-off workflow end to end on the
Microsoft 365 path: it lists a synthetic message's inline image, extracts the
exact decoded bytes, files the message through a hand-written manifest with no
template, and raster-inspects the saved PDF to prove the whole image is present.
It also checks that raw extraction leaves the filing verdict and mail state
untouched and that an unauthorized `save` writes nothing. A companion case runs
the same list-and-extract path through the read-only IMAP adapter and confirms
`save --execute` is still refused there. These cases run in required mode with
the same Chrome and Poppler dependency as the other renderer verification.

These checks complement the example-domain privacy guard; a clean secret scan does not establish the absence of personal preferences or identifying prose. Keep personal classifiers, configuration, mail, reports and operational notes outside the repository. Before publication, scan the final source snapshot and review repository history and any external artifacts separately.

## Generated documentation

`make docs` updates the package table in `docs/architecture.md` using the repository's Go generator. `make docs-check` verifies that the table matches package comments without writing the file. Missing, duplicated or reversed region markers fail validation. No external documentation or workflow tool is required.

This check covers only the declared generated region. Review prose against CLI
help, implementation and tests, and check relative links separately. For changing
source-derived values, prefer links to their source over copied limits or command
inventories. Keep ADR decision context historical; use focused operational guides
for current behavior. See [the documentation index](README.md).

For source tags, Git identities, tap updates and installed-binary verification,
read [releases](releases.md). A successful checkout build does not upgrade Homebrew
or prove which executable is selected on `PATH`.
