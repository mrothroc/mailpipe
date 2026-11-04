# Source and Homebrew releases

Publishing source, updating a Homebrew formula and installing locally are separate
delivery steps. Track their outcomes separately; a project tracker status alone
does not establish that any of them completed. Keep private operational reports
outside the public repository.

## Review the exact source

1. Review the final diff, public fixtures, documentation and generated regions.
   Run `make check` with the required browser/PDF inspection dependencies. Inspect
   all failures; unavailable metadata, sandbox refusals or missing tools are not
   successful security or rendering verification. See [development](development.md).
2. Confirm the commit and tag will contain only reviewed source. Inspect ignored
   and untracked files before constructing any release archive. Source privacy
   review includes identifying prose and preferences, beyond scanner findings.
3. Check repository-local Git identity before committing in both this repository
   and the tap. Use a public identity approved by the owner, such as the exact
   GitHub-provided noreply address. Inspect the resulting commit's **author and
   committer**, and the annotated tag's **tagger**, before pushing. Do not rely on
   the machine's inferred identity or change global Git settings unnecessarily.
4. Verify required CI against the exact reviewed commit, then create the approved
   tag/release at that commit. Review release notes and assets for private data;
   do not upload local configuration, database backups, logs or scan reports.

Changing the latest commit does not erase previously hosted commits, pull-request
references, logs or cached objects. A history rewrite needs a separate publication
review; do not describe tip cleanup as removal of all earlier disclosures.

## Update the Homebrew tap

The installation target is `mrothroc/tap/mailpipe`. Reuse the existing tap and
inspect its formula and contribution workflow
before editing it; do not introduce another distribution mechanism implicitly.

- Verify the public tag's source archive against the intended source commit and
  compute its checksum from the downloaded bytes. A valid checksum alone does
  not establish that the archive contains the intended code.
- Update the formula's source URL/checksum through the tap's normal bump workflow.
  Preserve build, example installation and smoke-test behavior; run its required
  style, audit, build and test checks.
- Review the formula pull request and its CI on the exact final revision. A tap's
  post-merge syntax check is not a substitute for formula installation tests.
  Confirm the merged formula contains the reviewed changes.

The current distribution builds from source using Homebrew's Go dependency.
It does not require a prebuilt signed application download. Adding bottles or
other binary assets is a separate packaging decision.

## Verify installation

After the tap is updated, upgrade with `brew upgrade mrothroc/tap/mailpipe` and run
`brew test mrothroc/tap/mailpipe`. Confirm the installed formula version and resolve
`command -v mailpipe` through any symlinks to the intended Homebrew executable.
An older executable earlier on `PATH` can hide a successful upgrade.

`make check` builds the checkout's `bin/mailpipe`; it does not install that binary
or upgrade Homebrew. Use the explicit checkout path to test source changes. Neither
installation nor a smoke test authorizes mailbox execution or migration recovery;
read [operations](operations.md) and [recovery](recovery.md) for those workflows.
