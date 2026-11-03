# Complete inline images in filed PDFs

Status: accepted technical design.

## Context and decision

Both compatibility `Doc.OfflineHTML` and streaming `FileDoc.OfflineHTML` feed
`render.Chrome.PDF`. Intrinsic raster dimensions and email table widths previously
allowed images to exceed printable bounds; a synthetic 600 by 3600 portrait
produced five pages before this change. The shared renderer owns fitting, rather
than sender templates or either individual document builder.

The trusted print layout uses Letter paper (612 by 792 points), 36-point margins
and a 540 by 720-point printable area. Images have a maximum border-box width of
the smaller of their container and 540 points, and maximum height of 648 points.
The remaining 72 points provide space for ordinary source headers and line-box
rounding. Long actual headers/text remain complete and can consume more pages.

After allowlist sanitization, renderer-owned image styles constrain min/max sizes
and use `object-fit:scale-down`. Intrinsic content keeps its aspect ratio even
when an authored width/height pair conflicts. Small authored image boxes remain
small. Image-bearing containers receive bounded widths; image-only wrappers lose
fixed heights and overflow clipping. Image-bearing tables use fixed layout within
printable bounds, retaining smaller literal pixel width hints. These are narrow
presentation overrides, not a general CSS rewriting engine.

Only individual images avoid internal page breaks. Containers stay fragmentable.
Standalone image runs use block boxes so adjacent replaced elements do not create
an oversized inline line box. Runs between block siblings are detected even when
the same parent contains source headers; images embedded in ordinary text remain
inline.
There is no forced page break before every image, whole-message unbreakable group,
transform scaling or whole-page rasterization. Source fonts, colors, links and
ordinary text/table structure remain.

The two document builders share escaped From/Date/Subject presentation and
wrapping plain text. Streaming CID embedding retains presentation hints while
writing base64 in chunks into the existing quota-charged page blob. It does not
decode image pixels or materialize attachments. Chrome.PDF still sanitizes every
page, puts CSP first, permits raster data images only, and uses the existing dead
proxy, timeout and private throwaway profile. Existing complete-PDF handling for
Chrome's macOS nonzero exit remains. ADR0005 content limits, cancellation, cleanup
and filing publication contracts are unchanged; no browser RSS ceiling is claimed.

## Verification

`make test` and therefore `make check` set `MAILPIPE_REQUIRE_RENDER_TESTS=1`.
Chrome and Poppler are mandatory in release verification. CI explicitly checks
executable Chrome and its version, `command -v pdftotext` and
`command -v pdftoppm` before `make check`. Test preflight checks tool versions and
required tests actually render/convert PDFs. Missing/unusable tools, conversion
failure and missing output fail actionably; there is no successful required-mode
skip. Older standalone renderer tests retain optional dependency behavior only
outside required mode.

Tests generate reserved-domain multipart MIME and raster images with four edge
bands, asymmetric corners and an interior circle. They exercise portrait,
landscape, both-dimension oversize, conflicting dimensions/important styles,
oversized table wrappers, multiple images, and ordinary text/table/small-logo
content through both compatibility rendering and real offline CLI filing.
Poppler rasterization checks every page and complete edge bands, orientation,
content aspect ratio, printable bounds and pagination. Text bounding boxes and
raster ink verify source headers. Corrupted edge/aspect/blank-page controls test
the inspector itself. Dependency subprocess tests prove absent Chrome/pdftoppm/
pdftotext fail at the actual required test entry point.

## Limits and alternatives

Very tall scans shrink to remain complete; no OCR, slicing or sender policy is
introduced. This does not promise visibility against arbitrary intentionally
hidden mail CSS. CSP/allowlisting, rather than presentation overrides, enforces
the resource boundary. Actual Chrome/Poppler output is the pagination oracle;
small antialias tolerances are checked with negative controls, not relaxed to
accept clipped content.

Width-only fitting misses tall images; height-only fitting misses wide ones.
Document-only rules miss sibling callers. Transform scaling leaves pagination
boxes oversized, blanket layout flattening harms ordinary mail, and pixel
resampling duplicates the browser while raising memory cost. These alternatives
are rejected. No public APIs, dependencies, provider behavior, mailbox writes or
generated architecture change is needed.

Source `@page` rules are removed before installing the trusted geometry, including
named pages, pseudo pages, conditional rules and escaped page keywords. Merely
appending a plain page rule cannot override a more specific source rule. The
narrow CSS scan preserves ordinary rules, quoted strings, comments and function
arguments; it does not change the existing resource policy.
