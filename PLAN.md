# Roadmap

mailpipe separates mailbox synchronisation, classification, user policy and reviewed actions. Personal operational notes and deployment history belong outside this repository.

## Implemented capabilities

- Microsoft 365 device-code authentication with credentials in the macOS Keychain.
- Incremental Microsoft Graph synchronisation into account/provider-bound SQLite.
- Provider-neutral identity, membership, sync lifecycle and capability contracts.
- Read-only Yahoo IMAP setup, complete sync/content and per-account processing consent.
- Built-in stages and explicitly configured external classifiers.
- Optional recipient-address filtering for users who configure it.
- Private classifier hooks with scoped MIME access and bounded run costs.
- User-configured retention, sender rules and numbered action plans.
- Server-side managed blocking and junk rules.
- Template matching, review queues and PDF filing with complete inline images.
- MIME attachment listing and selected local extraction; one-off filing without templates.
- Structured CLI output and resumable operations.
- Owner-reviewed Graph history reconciliation for unavailable references with
  complete read evidence and verified live anchors; preserved local identities
  and audit history. See [reconciliation](docs/graph-reconciliation.md).

See [architecture](docs/architecture.md), [stages](docs/stages.md), [hooks](docs/hooks.md), [policy](docs/policy.md), [filing](docs/filing.md) and [operations](docs/operations.md).

## Future work

- Additional reviewed evidence policies for all-unavailable or ambiguous Graph
  histories; current reconciliation deliberately refuses those cases.
- Writable Yahoo actions and server-policy parity, with explicit capabilities.
- Gmail integration; no Gmail adapter is currently implemented.
- Optional scheduling with explicit execution and review policies.
- Alternative deployment targets and credential stores.
- Further adversarial evaluation of model-dependent classifiers.

## Public repository boundary

Examples and tests use invented data. Private senders, recipient addresses, vendors, folder structures, retention choices, classifier implementations and live-run reports are not part of the public project. Git history must receive a separate publication review; cleaning the current tree does not clean previous commits.
