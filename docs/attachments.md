# Inspecting and extracting MIME files

`mailpipe attachments list <n>` lists actual attachment entities and inline MIME
images from one account-bound local message number. It reads complete source
through the configured provider, including when the message's attachment flag is
false. It does not render the message. Run `mailpipe attachments --help` for the
command group and either leaf's `--help` for flags and exit codes.

```sh
mailpipe attachments list 42
mailpipe attachments list 42 --kind inline --json
mailpipe attachments list 42 --kind attachment
```

The default `--kind all` includes both attachments and inline images. Image kinds
follow the existing parser rule: an explicit attachment disposition wins;
otherwise a CID-bearing image is inline even when named; otherwise a named image
is an attachment. Unnamed images without CID, including root images, are retained
as inline images. Ordinary text/HTML bodies are not files
unless attached or named. An explicitly attached multipart container or nested
message is one opaque attachment; its descendants are validated, not separately
selected. An unattached nested message can expose its child files. Archives are
never expanded and images are never decoded or transformed by extraction.

Rows include `provider_id`, `account_id`, `n`, `part`, `mime_path`, `kind`,
`filename`, `content_type`, `disposition`, `cid`, `decoded_bytes`, `sha256`,
`output_name`, `extractable`, `status`, and `error_code`. `filename` and `cid` are
untrusted metadata. Sizes and SHA-256 digests describe the exact transfer-decoded
bytes. The MIME tree path describes source order, not a filesystem path. Duplicate
names do not collapse parts. Unsafe names can be listed with `extractable=false`
and `error_code=unsafe_path`; they cannot be extracted.

## Selecting an exact snapshot

Copy the full `part` selector from a current listing. A selector starts with `v1.`
and contains a SHA-256 digest binding the provider/account, local number, opaque
message identity and locator, entire raw MIME snapshot, MIME path and decoded
digest. It is neither a password nor permission to act on mail. Names, Content-IDs
and numeric positions alone are not selectors. Filtering does not renumber them.

**Re-list if the server reserializes MIME before extraction**, even if the visible
message looks identical. Changes to headers, boundaries, content or message
identity invalidate old selectors. Extraction fetches and validates source again;
it never silently substitutes the same filename or position. Use a bound database
and a configured account; legacy unbound databases need explicit account binding.

## Checking and authorizing local output

`--out-dir` is mandatory. There is no default destination, including the current
directory or a private mail directory. Create a private directory yourself, then
check the selected parts before executing:

```sh
mkdir -m 700 extracted
mailpipe attachments extract 42 --part '<full-selector-from-list>' --out-dir extracted --json
mailpipe attachments extract 42 --part '<full-selector-from-list>' --out-dir extracted --execute --json
```

Repeat `--part` for multiple selections; do not repeat the same selector. There
is no implicit “all” selection. Absolute user-selected output directories are
allowed. Relative roots resolve from the invocation's working directory; the
application does not expand `~`. The directory must already exist, be owned by
the effective user with mode **0700**, and have a real directory as its final
component. A symlink root, missing root, regular file or broadly accessible
directory is refused. Existing directories are never chmodded. Ancestors are
canonicalized and the verified root descriptor is pinned for all output I/O.

Check mode (without `--execute`) fetches, parses and hashes complete content and
checks destination/name/collision preconditions. It creates no output-root files,
temporary outputs or directories. The content layer still needs bounded private
0700/0600 temporary spools; they are cleaned before success. The shared `--limit`
metadata overflow contract also still applies.

Executing authorizes only the explicitly selected local file writes. Files have
mode **0600** and flat deterministic names. A safe original name becomes
`part-<mime-path-with-hyphens>-<original-name>`. Missing names use the same path
prefix with `.png`, `.jpg`, `.gif`, `.webp`, `.pdf`, `.eml`, or `.bin` according to
a fixed media-type map. This prevents duplicate MIME names from colliding with
one another. Extensions and content remain untrusted; no file is implicitly
opened, executed or rendered.

Untrusted absolute, traversal, slash/backslash, drive/UNC, dot, control/bidi,
invalid UTF-8 or oversized MIME filenames are rejected, rather than repaired by
taking their basename. Those restrictions apply to MIME names, not legitimate
absolute `--out-dir` roots. Every existing target entry is a collision, including
identical files, directories, hardlinks and contained or dangling symlinks.
Extraction never follows a target symlink or replaces an existing entry.

All selected files are first streamed into exclusive unpredictable private stage
files under the pinned root, synced, closed and verified by bounded readback.
Content spools and owned provider/store handles close before any final file is
published. No incomplete file can be published as a successful extraction. Final
publication uses no-replacement links followed by final-file readback through the
pinned root, as in the filing writers. Extraction also retains the verified stage
inode and checks the destination's identity, user ownership, regular-file type,
0600 mode, size and decoded digest. A bounded terminal check repeats those checks
after cleanup and directory synchronization, including earlier files in a failed
batch. Normal failures and cancellation clean identity-proven owned temporary
files; cleanup failures are surfaced rather than reported as
success. Crash leftovers need explicit operator inspection; there is no janitor
that adopts directories based on their names.

Multiple final links are **not one atomic filesystem transaction**. A late
collision, cancellation or cleanup error can follow earlier complete published
files. The command exits nonzero and reports those rows as `published`, with
`saved_path`, while failed/untouched selections report `failed`/`not_saved`.
Inspect those results before retrying: occupied destinations still fail collision.
No automatic overwrite or rollback of unrelated entries occurs.

If a destination is missing, substituted, modified or cannot be verified, its row
reports `failed`, leaves `saved_path` empty and directs inspection of
`planned_path`. A failed link acknowledgement can still leave a complete file;
only a destination verified against the retained stage identity and decoded digest
can be reported as `published`. Unexpected replacement entries are preserved;
cleanup removes a temporary name only after observing the created inode there.
An ownership/identity proof failure remains an error and can require manual
inspection of retained entries.

These checks validate observations at publication and terminal readback. They do
not lock out another process running as the same user, make a multi-file batch
atomic, provide atomic compare-and-unlink, or prevent modification after the last
check. A stable private namespace remains required. Cancellation stops further
links; bounded five-second reconciliation reads may finish checking already
attempted writes so uncertain artifacts are never presented as verified files.

## One-off filing without templates

Listing and extraction are useful on their own, and they also start a one-off
filing workflow that needs no templates. Templates make a recurring decision once
per kind of email; a single message can instead be filed from a hand-written
manifest:

1. `mailpipe attachments list <n>` and copy the full `part` selector(s) to keep.
2. `mailpipe attachments extract <n> --part <selector> --out-dir <dir>` to check,
   then `--execute` to write the exact decoded bytes. Extraction does not mark the
   message saved and does not approve deleting it; it only writes local files.
3. Write a save manifest — the header `n<TAB>folder<TAB>name`, then one row per
   message — and run `mailpipe save --manifest <file> --root <dir>` to check it.
   This is the same manifest [file draft](filing.md) writes from templates; a
   template is optional for a one-off record.
4. Only `mailpipe save … --execute`, with the owner's approval for that specific
   save, writes a PDF into the filing tree and records a local `filing:saved`
   verdict. Without it, nothing is written and no verdict changes.

The filed PDF is separate from the extracted files: a PDF attachment is filed as
is, while a message without one is rendered offline. Extraction and `save` are
local-only: extraction writes the selected files, and `save` creates PDFs and
records a local `filing:saved` verdict. Neither mutates server mail or deletes it;
`save` requires the owner's approval for that specific save. Deleting mail is a
separate action that needs a reviewed plan and its own approval. Extraction never
opens, renders or executes what it writes, and its output files are not evidence
that the message was filed. Read-only providers such as Yahoo still refuse `save
--execute`; listing and extraction remain available there.

## Output and errors

Commands use the shared `github.com/mrothroc/agentio` contract: stdout contains TSV
with a header or one JSON object per row with `--json`; stderr contains hints,
`[error] ... Try: ...` and the normal success footer. `--limit` bounds stdout and
places additional metadata rows in the established private overflow file. Text
display makes terminal/bidi controls visible; JSON preserves decoded metadata as
escaped data. Raw MIME, payload bytes, spool paths, locators and secrets are not
output.

Extraction includes `planned_path`, `saved_path`, `status`, `error_code` and a
safe `detail`. A successful check reports `ready` with empty `saved_path`; a
successful execution reports `saved`. Failures have fixed structured codes,
including `usage`, `stale_identity`, `no_data`, `unsafe_path`, `collision`,
`incomplete_content`, `cancelled`, `cleanup` and `io_error`. Parser/server causes
are not printed as raw error text. Argument/flag failures detected by the shared
wrapper use its normal stderr usage/error contract.

| Exit | Meaning |
|---|---|
| 0 | Complete successful listing, check or extraction |
| 1 | Runtime, unsafe destination/name, collision, incomplete content, cancellation or cleanup failure |
| 2 | Usage, including absent destination/selection or malformed selector |
| 3 | Stale account/message/part identity, unconfigured account or signed-out credentials |
| 4 | No matching parts |

The finite content limits remain those of [ADR 0005](adr/0005-bounded-imap-content.md):
64 MiB raw, 128 MiB cumulative decoded, 32 MiB per decoded part, 256 MiB aggregate
spools, 256 MIME nodes, depth 20, and a two-minute operation timeout by default,
plus the existing independent header/memory/text limits. No unlimited CLI override
is added. Output staging has a separate aggregate cap of 128 MiB and a per-file
cap of 32 MiB; it does not evade or masquerade as content-spool accounting. Copies
and hashes use at most 32 KiB chunks. These are finite logical resource budgets,
not a process RSS or machine-global disk guarantee.

The provider reads preserve unread state. Extraction makes no server mutations,
model calls, external resource loads, rendering calls or filing verdicts. It does
not mark a message filed, change pending/saved retention state, authorize deletion
or weaken Yahoo's existing `save --execute` refusal. Local files remain untrusted
and separate from reviewed PDF filing.
