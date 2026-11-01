# Yahoo read-only setup and configured principal identity

Status: accepted technical design.

## Composition

Absent provider still selects Graph. Yahoo uses a real `imapmail.Client` through
a narrow CLI read interface, with `Discover` and `SyncCollection` called directly
using `ContentMetadataLoader`. Existing `mailsync.RunPass` remains the only
publication lifecycle. IMAP does not fabricate page-returning `Provider.Sync`.
Graph retains its full provider, subject checks, page source, reviewed operations
and output fields. `CapabilitiesOf` requires only `Account`, while preserving the
same optional method assertions; reviewed-action dispatch contracts do not change.
Yahoo exposes no trash, archive, native-rule or managed-rule methods.

## Version 1 endpoint and opaque login

Input is `HOST` or `HOST:PORT` for implicit verified TLS. Omitted port means 993;
explicit ports are canonical decimal 1–65535, without signs or leading zeros.
DNS hosts have at least two ASCII LDH labels, each 1–63 bytes, starting/ending
alphanumeric, total at most 253 bytes, with a letter in the final label. Only
ASCII hostname letters are lowercased. Strict dotted-decimal IPv4 is also accepted
exactly as rendered by `netip.Addr.String`. Canonical output always includes port.

| Input | Canonical result |
|---|---|
| `MAIL.Example.org` | `mail.example.org:993` |
| `mail.example.org:993` | `mail.example.org:993` |
| `mail.example.org:1993` | `mail.example.org:1993` |
| `127.0.0.1:1993` | `127.0.0.1:1993` |

Reject whitespace, controls, non-ASCII, trailing dots, empty labels, underscores,
wildcards, single-label names, URIs/userinfo/paths/queries/fragments, percent or
backslash escapes, numeric IP shorthand, IPv6/brackets/zones, Unicode IDNA and
`xn--` labels. These are application restrictions, not a claim that every rejected
form is invalid DNS. No DNS/CNAME resolution establishes identity equivalence.
The exact canonical host is also the TLS certificate verification name; there is
no independent user override or insecure TLS option. Normal trust and TLS 1.2 or
later remain required. Generated test trust is injected only by tests.

Login is 1–254 printable ASCII bytes, preserved exactly, including case and
leading/trailing/internal spaces. Reject unsupported encoding instead of trimming,
case folding, parsing an email address or expanding aliases. DNS equivalence does
not imply login equivalence.

## Permanent configured-principal binding

Tuple `T` is ASCII `mailpipe-imap-principal`, NUL, big-endian uint32 version 1,
then three big-endian uint32 byte-length-prefixed strings: provider namespace,
canonical endpoint and exact login. Yahoo's namespace is `yahoo`. AccountID is
`imap-principal-v1:` plus unpadded base64url of `T`; ProviderID is `yahoo`. Encoding
is lossless and injective. Passwords never enter this tuple.

Configuration recomputes and checks AccountID. Existing AccountStore permanently
checks account AND provider when opening and in every transaction. Manually
changing AccountID therefore cannot adopt an old database after a principal
change. No schema, raw SQL escape or rebind operation is added. Empty-store opening
can establish configured attribution before authentication; it neither publishes
mail nor proves an immutable remote subject.

Changing endpoint, port, login or identity version requires an explicitly
different database. Restoring the exact original tuple can reopen its database;
same-principal password rotation preserves history. IMAP cannot detect service-side
reassignment of the SAME login to another owner: use a new database in that case.
Headers/display identities provide no continuity proof. Historical Graph
attribution remains Graph-only.

## Credential lifecycle and failures

`pkg/imapauth` owns identity, credential encoding and the Keychain session
abstraction; it owns no CLI, config persistence, protocol or database publication.
New service is `mailpipe-imap-v1`; item account is `principal-v1.` plus unpadded
base64url of `T`. Existing Graph/MSAL and OpenRouter names remain unchanged.
The secret envelope is compact JSON
`["imap-credential-v1", base64url(T), generation, password]`, without HTML escaping.
Generation is 128 random bits represented as 32 lowercase hex digits. Every read
checks exact version, cardinality, tuple, generation and input bounds.

Passwords are 1–256 printable ASCII bytes. Stdin accepts one LF/CRLF line, or
nonempty EOF without newline. Empty input, extra lines, controls, oversize input,
read failure and cancellation fail. Terminal input disables echo and restores the
exact previous state on every path, surfacing restoration errors. Nonterminal
input requires `--password-stdin`. No password flag, environment variable,
configuration field or fallback secret file exists. Errors omit raw collaborator,
server and Keychain output. Promptly clearing owned buffers does not guarantee
complete Go/OS memory zeroization.

The bounded envelope fits one existing Keychain item; worst-case actual
`security -i` stdin is below 4000 bytes including the lossless account name and
hex value. Passwords travel only on stdin, never argv. Existing chunked MSAL
storage is unchanged. One current-user credential lock serializes config/Keychain
handshakes and all configuration writers across configurations; it does not serialize mailbox runtime or replace
SQLite lifecycle locks. Changing HOME under the same UID is not another OS user.

Login checks database binding before input or remote reads, authenticates a
candidate using the real verified-TLS client, and closes the probe before
persistence. It durably saves nonsecret `ready:false` with a fresh generation,
writes the item, reads back exact tuple/generation/password, then saves
`ready:true`. The temporary config file is synced before rename; nonready state
also syncs its parent directory before any Keychain write. Ready publication keeps
rename as the last fallible step. A crash that loses that final rename exposes
nonready state and requires explicit sign-in, rather than adopting a secret.
Failed, partial or uncertain writes and final config failure cannot
acknowledge successful sign-in. Status requires matching ready generation and
envelope; stored credentials are not current remote authentication proof. Logout
first disables readiness, then deletes and verifies only that tuple's item.

Explicit verified login can repair truncated bytes at the exact injective key;
ordinary lookup never adopts or repairs them. Readable different tuples/unknown
credential versions are not overwritten. Read failures stop persistence. Recovery
is fresh-input login, never another item's secret or a fallback file. An
interrupted rotation can temporarily disable credential availability.

## Consent, effects and cleanup

Every fresh setup, Graph or Yahoo, disables account-scoped external processing.
`jev`/`template` require model consent; `exec`/`hook` require separate subprocess
consent even for metadata-only input. Checks precede key lookup, child creation
and message transfer. Deterministic stages, local fingerprints, content reads and
plan review do not require this consent. Trusted hooks retain their existing
wire/source/cost contract and OS permissions.

Pre-existing Graph configurations WITHOUT a processing record retain explicitly
configured legacy behavior. Once a record exists it is enforced for the exact
account. Yahoo never inherits that exception. Copying another account's record
grants nothing; authorizing one scope cannot import the other account's enabled
flags. Opt-out cannot recall an already admitted external call.

Yahoo `apply --execute`, `rules apply --execute` and `save --execute` fail before
store, credential, mail, rendering or destination-file effects, including empty
and already-saved cases. Trash checks and native server-rule commands report
their specific absent capabilities, not successful no-ops. Local plans and
scan-time policy intent remain usable. Dry save checks a manifest without
suggesting unavailable execution. Graph owner-approval requirements remain intact.

A command shares one lazy owned IMAP client across body/hook callbacks, joins
workers, then closes it and surfaces cleanup errors before success. Existing
operation scopes retain ownership; Client closes its fallback scope. Help and
cached offline paths do not authenticate to create/close an unused client.
Complete content, quotas, input-size guards and protective verdicts remain under
ADR 0005.

References: [DNS ASCII case rules](https://www.rfc-editor.org/rfc/rfc4343.html),
[Go TLS](https://pkg.go.dev/crypto/tls#Config),
[file synchronization](https://pkg.go.dev/os#File.Sync),
[IP parsing](https://pkg.go.dev/net/netip#ParseAddr),
[IMAP LOGIN](https://www.rfc-editor.org/rfc/rfc9051.html#section-6.2.3),
[Apple security manual source](https://raw.githubusercontent.com/apple-oss-distributions/Security/main/SecurityTool/macOS/security.1),
and [the pinned terminal syscall helpers](https://pkg.go.dev/golang.org/x/sys/unix).

Configuration routing and account-binding commands complete remote discovery outside
the credential/configuration session. Inside the session they reload and validate
the current provider, account and configured principal, then merge only their
account/collection routing changes. They preserve current processing authorization,
credential generation and unknown private fields. A changed principal or role
mapping requires a fresh command. Configuration initialization rechecks existence
inside the same session; processing updates merge into the fresh exact-account
configuration there. A stale snapshot can never restore legacy processing consent.
