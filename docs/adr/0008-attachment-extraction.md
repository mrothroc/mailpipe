# Provider-neutral attachment inspection and selected extraction

Status: accepted, 2026-11-03.

## Context

The existing bounded document parser retains attachments and CID-indexed inline
images for filing. The CLI has no supported way to enumerate or extract files
without PDF rendering. A message attachment flag cannot prove absence of inline
images; unnamed images without CID are currently discarded by ordinary parsing.
Filing's rooted writer intentionally permits identical/PDF-normalized existing
outputs, while standalone extraction must reject every collision.

The source and provider boundaries in ADRs 0001–0006 already answer transport,
identity, quota and cleanup questions. This decision extends that content context;
it adds no provider, credential mechanism, mailbox identity, schema or retention
action.

## Decision

Add `attachments list` and `attachments extract` through the existing
`newGroup`/`newAgentCommand` and `agentio.Tabled` CLI contract. List complete MIME
attachments and non-attachment images by default, with an explicit kind filter.
Read through `lazyProvider`, account-bound `remoteRef`, `document.OpenSource` and
one operation `Scope`. Never use attachment metadata as a fetch gate or construct
another Graph/auth transport path.

`ParsePartsReader` is an inspection retention mode on the same bounded MIME walk.
It provides ordered `InspectablePart` descriptors and Blob readers in
`FileDoc.Parts`, including unnamed/CID-less images. Keep the existing `FilePart`
shape, `ParseReader`, legacy attachments/inline maps, text selection and PDF filing
behavior. Explicitly attached containers/messages are opaque selectable entities
whose descendants are still completely validated. No second MIME parser or
payload-sized attachment materialization is introduced.

Image kinds follow the existing parser precedence: explicit attachment disposition
wins; otherwise a CID-bearing image is inline even when named; otherwise a named
image is an attachment. An unnamed image without CID is retained as inline by
inspection mode. Kind filters follow these same entities rather than redefining
the legacy attachment rule.

Each selector is `v1.` plus SHA-256 of a fixed-order JSON string array containing
the versioned domain, provider/account, local n, opaque message ID and locator,
complete raw-source digest, MIME path and decoded digest. This is selection
evidence, never a substitute for proven mailbox identity or execution authority.
Any MIME reserialization requires a fresh listing. Duplicate names and Content-IDs
do not select files; existing ambiguous-CID parser failures remain protective.

Extraction requires one or more full `--part` selectors and an explicit
`--out-dir`, with no default destination. Relative roots resolve against invocation
cwd; absolute roots are legitimate. Require an existing effective-user-owned 0700
directory with no final symlink, canonicalize trusted ancestors, verify and pin
its `os.Root` descriptor. Use flat deterministic MIME-path-prefixed names, fixed
fallback extensions and strict rejection of unsafe MIME names. Create no
directories and never chmod an existing one. Existing target entries always fail,
including identical files and all symlinks/hardlinks.

Without `--execute`, verify content and preconditions but create no output-root
files. Executing streams exact decoded bytes into exclusive 0600 stage files,
verifies size/hash/inode/readback, then closes content and owned provider/store
resources before final no-replacement linking. Output staging has a separate
finite aggregate decoded-byte cap. Rooted I/O and 32 KiB copying preserve the
existing filesystem pattern while avoiding filing's collision equivalence and
verdict effects. Unsupported ownership/platform evidence fails closed. POSIX
tests exercise real descriptor pinning, symlink confinement and directory sync;
the supported macOS release gate runs those operations directly.

Publish multiple files serially, without claiming atomic batch publication. Late
failure reports complete earlier files as `published` and fails the operation.
Cleanup errors remain nonzero; no incomplete output is successful and no unrelated
file is removed as rollback. Private namespace/same-UID and crash-recovery limits
remain those of ADR0005. Never implicitly open or execute extracted content.

Structured rows and safe errors retain shared TSV/JSON stdout, stderr hints and
footer, exit codes 0/1/2/3/4 and private metadata overflow. Explicit `unsafe_path`
and `collision` rows return runtime exit 1. Extraction neither renders nor makes
model/network-resource calls, changes read state, records filing:saved, updates
retention policy or permits mailbox/server mutations.

## Alternatives and consequences

Graph attachment endpoints would bypass the shared MIME/provider boundary.
Filename/CID/index selection could silently target changed content. Whole-message
materialization would duplicate bounded streaming work. Globally retaining more
images would alter unrelated filing/text consumers. Reusing the filing writer
would accept collisions and couple local extraction to saved state. An implicit
destination would hide the local-write boundary. These alternatives are rejected.

Conservative selectors can become stale after harmless MIME serialization changes;
users re-list instead of accepting a guess. Private existing roots require an
explicit mkdir step. Multiple publication links can leave complete partial batch
results on failure. Those limits are visible in help, structured results and
[the command guide](../attachments.md).

Synthetic tests cover exact decoded byte hashes, unnamed inline-only messages,
mixed/nested entity boundaries, compatibility consumers, malformed/over-limit
content and cleanup, account/message/part staleness, destination/path/collision
matrices, descriptor replacement, staged write/sync/close/readback faults, late
cancellation, output sanitization and real local HTTP/verified-TLS read-only
provider composition. Existing parser/provider/filing tests and `make check`
remain required. All fixtures use reserved domains and generated content; no live
mailbox, model or personal configuration is used.
