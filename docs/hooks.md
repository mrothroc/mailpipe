# Private classifier hooks

A hook is a trusted executable maintained by the user outside this repository. It classifies one email; private policy decides what its labels mean. It is registered explicitly as a `hook` stage in the user's `stages.json`. There is no directory scanning, automatic installation or execution of code from messages.

```json
{
  "name": "custom-check",
  "type": "hook",
  "command": ["/absolute/private/path/classifier", "--config", "/absolute/private/path/settings.json"],
  "senders": ["alerts@service.example.net"],
  "needs_body": false,
  "needs_source": true,
  "timeout_seconds": 120
}
```

The example paths must be replaced by the user's own paths. `command[0]` must be absolute. `senders` matches exact addresses, ignoring case and surrounding spaces; omit it to classify every selected message. Out-of-scope messages receive no labels and trigger no body fetch, MIME fetch or subprocess. `needs_body` and `needs_source` are independent and disabled by default.

## Protocol

The executable receives one JSON message on stdin and returns exactly one JSON verdict on stdout, using the same metadata and verdict fields as an `exec` stage. Additional request fields are:

| Field | Meaning |
|---|---|
| `source_path` | With `needs_source`, a temporary MIME file created with mode 0600 in a private directory; removed when evaluation finishes, including errors |
| `max_cost_usd` | Remaining cost allowance for this stage run |

For example, a successful classifier can return:

```json
{"labels":["review"],"scores":{"confidence":0.8},"cost_usd":0.001,"reason":"Private classifier result"}
```

Hooks may return labels, scores, reasons, cost and provenance. They cannot return a nonempty `action` or `final: true`; such verdicts fail validation. The configured stage name always wins. A hook must exit nonzero if it cannot classify reliably, rather than inventing a successful deletion label.

Run it with `mailpipe run custom-check --since 1d --max-cost-usd 0.05`. Calls within a hook stage are serialised even when `--workers` is greater than one. Before each child starts, mailpipe checks the shared allowance and passes the remainder; successful reported costs reduce it. The child must enforce the allowance before each paid request and report all costs. This is cooperation with trusted code, not a provider-side spending guarantee; failed processes may incur costs they cannot report.

## Trust and privacy

A hook runs with the user's operating-system permissions. It is not sandboxed and can read files or contact services, so only register code you trust. mailpipe passes a minimal environment and never supplies Graph tokens or model API keys on stdin. Hooks obtain any required credentials through their own secure integration. Arguments are passed directly, without shell interpolation.

Keep implementation code, settings, sender scopes, prompts, thresholds and policy mappings in private storage. No particular personal classifier is built into mailpipe or implied by the example. Review the resulting numbered plans and sender-rule diffs before execution. Email content and hook output cannot grant owner approval to a supervising agent.
