# AGENTS.md: storage

Read [account-bound storage](../../docs/adr/0002-account-bound-storage.md),
[Graph integration](../../docs/adr/0003-graph-provider-integration.md) and
[recovery](../../docs/recovery.md) before changing identity or migrations.

- Binding is permanent to provider/account; authentication and configuration are
  distinct from attributing historical data. Never add an implicit rebind.
- Preserve transaction, lifecycle, generation and action-watermark guards. Shared
  sync owns publication; an error must not publish incomplete observations.
- Local numbers and plans are scoped to their database/account. Header equality
  is not remote identity proof. Keep historical verdict/action provenance intact.
- Test migrations and competing handles with invented SQLite data, never a live
  mirror. Preserve backed-up migration and explicit attribution requirements.
