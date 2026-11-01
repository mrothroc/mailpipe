# Provider acceptance and candidate delivery

Status: accepted technical design; live acceptance and candidate CI results must be verified separately.

## Decision

Acceptance has two stages. Stage 1 uses invented fixtures and required CI. Stage 2
is an owner-attended Yahoo read-only session after a tested, reviewable candidate
and specific account-owner consent. A synthetic test named `TestYahooReadOnlyPilot`
is not a live pilot. Missing live access blocks final release acceptance.

The existing release gate continues to test GitHub's pull-request merge result.
A separate required `candidate_formula` job checks out the explicit PR head SHA
(or event SHA for other triggers), validates it and runs the full release gate
again before source installation. The archive, helper, formula, installed smoke,
review and recorded candidate must identify that exact commit. Main is checked
again after merging. A moving branch or an unpublished remote tag is insufficient.

`tools/acceptance` is developer/owner acceptance tooling. It imports no mailpipe
runtime, provider, credential or storage package. Its Bash wrapper owns a private
archive and a direct-child server that continuously holds `127.0.0.1:0`. A bounded
readiness handshake verifies PID, candidate and SHA256; only the exact archive is
served. Shell traps precede children. Phase supervisors own their process groups;
cleanup stops and reaps only owned children. One shared deadline includes child
joins, the three Brew cleanup operations, Go cache cleanup and final root removal;
per-command limits cannot extend it. Budget exhaustion or uncertain joins fail,
without claiming that kernel-blocked operations are forcibly interruptible. Cleanup
removes owned files and Brew
resources. `TestCandidateFormulaServerLifecycle` exercises the actual wrapper,
including parallel instances, failures, signals and timeouts.

Real Brew installation runs only on the disposable GitHub-hosted `macos-15` job,
using its default prefix. Temporary HOME isolates CLI files and user trust/config;
it does **not** isolate Cellar, prefix, taps or the OS Keychain. Ordinary local
installation through this harness is refused before any Brew command. Candidate
trust is limited to the single generated formula in a unique temporary tap.
Missing/different trust interfaces or broader trust readback fail before install;
no global trust or disabled guard is used. The normal tap is never changed here.

The current ordinary tagged formula asserts `Microsoft 365` in help. Current
candidate help says `Graph` and `read-only Yahoo`; the candidate test checks those
actual strings. A later approved normal-tap publication must update that assertion
alongside the released tag/checksum. Candidate validation does not publish a tag,
release, tap change or deployment.

The declared validation baseline is `macos-15` only, pending an actual successful
required install run at the candidate. No additional macOS or architecture support
is inferred. CI records actual technical provenance and seven fixed timing phases.
An absent completion/cleanup record after abrupt termination is incomplete evidence,
never a successful zero-duration phase. Root must inspect actual required logs and
the 30-minute job budget. Disposable VM destruction contains uncatchable failures;
SIGKILL cannot run shell traps.

## Private evidence

Consent and actual pilot receipts remain outside every repository/worktree in an
explicit private root, directories 0700 and files 0600. Descriptor-relative checks
reject aliases, links, repository ancestors, unsafe modes and namespace changes.
The owner reviews private evidence in their session. Public summaries construct a
new fixed schema with independently verified candidate identity, closed enums and
booleans; actual counts, consent references and owner choices never transfer.
Canary tests and manual review precede any publication. See the seven operational
sections in [operations](../operations.md).

## Alternatives and consequences

An ordinary local Brew install risks personal taps and Cellar even with a changed
HOME. A relocated prefix introduces unproved bottle/build behavior. A remote tag
would make pre-publication testing depend on publication. A free-port probe races;
a directory HTTP server exposes unrelated files. Filtering a private map by key
can leak private values through otherwise allowed fields. These approaches are
rejected. Separate required CI costs another runner job but preserves the original
merge gate and ties packaging evidence to the exact reviewed source.

References: [GitHub event/checkout semantics](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request),
[Homebrew command/trust interfaces](https://docs.brew.sh/Manpage),
[Homebrew trust JSON implementation](https://github.com/Homebrew/brew/blob/main/Library/Homebrew/cmd/trust.rb),
[Homebrew installation](https://docs.brew.sh/Installation),
and [Go module verification](https://go.dev/ref/mod#go-mod-verify).
