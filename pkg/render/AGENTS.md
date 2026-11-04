# AGENTS.md: rendering

Read [complete image filing](../../docs/adr/0009-complete-image-filing.md) and the
[security verification guide](../../docs/development.md) before changing rendering.

- Keep parsed sanitization and restrictive CSP ahead of untrusted content;
  browser flags alone do not establish isolation.
- Preserve offline resources, full image content, aspect ratio and visible source
  headers. Extraction and rendering are separate operations.
- Verify with real browser and PDF inspection tools through the required test
  mode. Missing dependencies or a sandbox refusal are not successful verification.
