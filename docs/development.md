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

The [CI workflow](../.github/workflows/ci.yml) runs `make check` on pushes to `main`, pull requests, every Monday at 11:23 UTC, and manual dispatch. It uses a standard `macos-15` runner, installs Poppler, and requires the installed Chrome binary and `pdftotext` before running tests, so browser security tests cannot silently skip because their dependencies are missing.

CI uses the latest stable Go compiler; the release gate still checks that both that compiler and the module's minimum Go family are supported. Newly disclosed dependency vulnerabilities or an unsupported minimum Go version therefore fail the weekly run even without a source change. Scheduled runs use the default branch, can be delayed, and GitHub automatically disables schedules in public repositories after 60 days without repository activity; re-enable the workflow in Actions if that happens. See [GitHub's schedule documentation](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule).

The workflow has read-only repository permissions, SHA-pinned official actions, no persisted checkout credentials, and no dependency caches or uploaded artifacts. Fork contributions use `pull_request`, with GitHub's normal approval rules. CI needs no mailbox credentials, model API keys or private configuration; all fixtures are invented. These jobs build and test the project and never operate a real mailbox. Logs are public. The example-address privacy guard also covers workflow YAML under `.github`.

## Publication boundary

The renderer security tests use invented HTML and temporary canary files. When Chrome and Poppler's `pdftotext` are installed, the test suite also checks the resulting PDF for script execution and local-file inclusion using the actual browser. That integration test skips when either optional tool is unavailable; the sanitizer unit tests always run. Browser flags alone do not enforce the rendering boundary: parsed HTML sanitization and an early Content Security Policy do.

These checks complement the example-domain privacy guard; a clean secret scan does not establish the absence of personal preferences or identifying prose. Keep personal classifiers, configuration, mail, reports and operational notes outside the repository. Before publication, scan the final source snapshot and review repository history and any external artifacts separately.

## Generated documentation

`make docs` updates the package table in `docs/architecture.md` using the repository's Go generator. `make docs-check` verifies that the table matches package comments without writing the file. Missing, duplicated or reversed region markers fail validation. No external documentation or workflow tool is required.
