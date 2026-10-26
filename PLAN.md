# Roadmap

mailpipe separates mailbox synchronisation, classification, user policy and reviewed actions. Personal operational notes and deployment history belong outside this repository.

## Implemented capabilities

- Microsoft 365 device-code authentication with credentials in the macOS Keychain.
- Incremental Microsoft Graph synchronisation into SQLite.
- Built-in stages and explicitly configured external classifiers.
- Optional recipient-address filtering for users who configure it.
- Private classifier hooks with scoped MIME access and bounded run costs.
- User-configured retention, sender rules and numbered action plans.
- Server-side managed blocking and junk rules.
- Template matching, review queues and PDF filing.
- Structured CLI output and resumable operations.

See [architecture](docs/architecture.md), [stages](docs/stages.md), [hooks](docs/hooks.md), [policy](docs/policy.md), [filing](docs/filing.md) and [operations](docs/operations.md).

## Future work

- Optional scheduling with explicit execution and review policies.
- Alternative deployment targets and credential stores.
- Further adversarial evaluation of model-dependent classifiers.

## Public repository boundary

Examples and tests use invented data. Private senders, recipient addresses, vendors, folder structures, retention choices, classifier implementations and live-run reports are not part of the public project. Git history must receive a separate publication review; cleaning the current tree does not clean previous commits.
