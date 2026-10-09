# Report and announcement template collection

Eight standalone LightCMS templates, with embedded CSS and JavaScript, plus the coordinated `crypto-analysis.html` layout. These are reusable layouts, not reports or official exchange announcements.

The curated local library contains these eight layouts plus `crypto-analysis` (nine total). All library names, descriptions, categories and field display labels are Chinese; field identifiers stay unchanged. The crypto template now has its own `.template.json` and `.example.json` import files. Its body is **richtext**, not Markdown, to preserve compatibility with existing articles; it declares eight fields (five article fields and three share/icon fields). The other eight layouts declare fifteen fields each.

Compatibility for existing crypto articles: the renderer converts a `crypto-analysis` body that contains a Markdown heading and no HTML tags through its frozen Markdown pipeline. Authored richtext HTML remains HTML; this exception does not change other templates or field types. Prefer valid HTML for new richtext submissions. Article data is not rewritten by this compatibility rendering.

The local migration preserves custom template IDs and imports revised contracts as new versions. The persisted site setting `settings.type=template_library_policy`, `custom_only=true` disables seeding stock templates and stock pages during `SeedDefaults`. Sites without this setting keep the original default-install behavior. Do not remove this setting or deploy an old binary if the library must remain custom-only; an old binary would seed the seven stock templates again. Migration and backup details are recorded in [the cleanup report](../../docs/reviews/2026-10-04-curated-template-library.md).

## Available styles

| Slug / HTML file | Visual direction | Suitable content |
| --- | --- | --- |
| [cybersecurity-report](cybersecurity-report.html) | Dark ink, mint accents, monospace labels, left-hand navigation | Threat intelligence, incident reports, security research |
| [fund-research](fund-research.html) | Ivory and forest green, serif headlines, research sidebar | Fund reviews, allocation and investment research |
| [venture-research](venture-research.html) | Warm paper, plum masthead, editorial callouts | Venture theses, startup and industry research |
| [binance-announcement-style](binance-announcement-style.html) | Charcoal masthead, restrained yellow accents, single-column notices | Exchange-style announcements |
| [okx-announcement-style](okx-announcement-style.html) | High-contrast black and white, bold headings, strong rules | Product and trading notices |
| [editorial-news](editorial-news.html) | Newspaper masthead, cream paper, readable serif body | General news and long-form reporting |
| [financial-daily](financial-daily.html) | Salmon paper, navy masthead, financial editorial typography | Markets, economics and business reporting |
| [technology-report](technology-report.html) | Midnight blue, oversized modern headings, field-note layout | Technology, infrastructure and product research |

The exchange-inspired layouts use independent publisher names and original icons. They are not official Binance/OKX pages and contain no official logos. Replace the publisher fields with your actual identity, not an implied affiliation. Design differences include typography, page structure, navigation placement and article treatment, rather than color alone.

Each style ships with:

- `<slug>.html`: Go `html/template` layout to store as `HTMLLayout`.
- `<slug>.template.json`: complete template-creation request, including the exact same HTML and all field definitions.
- `<slug>.example.json`: valid draft generation request with clearly illustrative English and Chinese content.
- `static/images/report-templates/<slug>-share.png`: 1200 × 630 original social cover.
- `static/images/report-templates/<slug>-icon.svg`: favicon.
- `static/images/report-templates/<slug>-touch.png`: 180 × 180 touch icon.

## Field contract

All eight layouts use the same data contract. Unknown fields are rejected by the product validator; import the definitions before sending data.

| Fields | Type | Required | Behavior |
| --- | --- | --- | --- |
| `headline` | text | Yes | English article title and server-rendered social title |
| `summary` | textarea | Yes | English introduction and social description |
| `body` | markdown | Yes | English Markdown body; tables, lists, links and headings supported |
| `author`, `category` | text | Yes | Author identity and English category |
| `headline_zh`, `category_zh` | text | No | Stored Chinese overrides (not auto-selected; English renders by default) |
| `summary_zh` | textarea | No | Chinese introduction |
| `body_zh` | markdown | No | Chinese Markdown body |
| `publisher_name`, `publisher_name_zh` | text | No | Actual publisher identity; generic style name if omitted |
| `share_image_url` | url | No locally; mandatory at go-live | Public absolute HTTPS URL; image must actually be 1200 × 630 |
| `share_image_alt` | text | No | Image description; falls back to the English headline |
| `favicon_url`, `touch_icon_url` | url | No locally; configure at go-live | Public absolute HTTPS icon URLs; local `/static/` fallback when omitted |

`published_at` and `public_url` come from the renderer. Never put them in `data` or declare them as template fields. Omit optional values or use empty strings; do not send JSON `null`. URL fields permit only HTTPS. Example `publisher.example` URLs are placeholders, not deployed assets.

These manifests deliberately use only the fields supported by the current template-create endpoint: `name`, `slug`, `category`, `description`, `fields`, `html_layout`. Library names, descriptions and categories are Chinese; the article layouts remain English-first. Newly created templates default to active, and script policy is inherited from the system. The layouts include trusted inline JavaScript; do not configure a hosting CSP that blocks it without supplying appropriate script hashes. No external fonts, CDN scripts or JavaScript libraries are needed by the generated page.

Field `label` values are also Chinese. For example, `headline` is displayed as “英文标题”, `body` as “英文正文（Markdown）”, and `share_image_url` as “社交预览图地址（1200×630）”. Labels are admin-facing metadata and do not rename placeholders or translate article values.

## Language and initial loading

The initial HTML and social metadata are English, and English is the runtime
default for every browser: the page never auto-switches to Chinese regardless
of `navigator.language`. Chinese article/interface strings stay stored in the
`*_zh` fields and message dictionaries but are inert until an explicit UI
language toggle is added. Article values are **not machine-translated**;
author names are content values, not translated UI labels.

The page remains hidden for a minimum 500 ms after its CSS animation starts. A loading screen is shown during that interval. This is a presentation delay, not deferred network loading: the article and metadata are already delivered in the response. Reduced-motion users retain the delay without the moving indicator. With JavaScript disabled, CSS still reveals the English article after 500 ms and hides nonfunctional action buttons. Printing bypasses the loading screen.

The script builds a table of contents from the selected article, calculates approximate reading time, offers copy-link with a visible failure fallback, and provides back-to-top and reading progress. Small screens switch to one column, with long tables/code blocks scrolling within the article.

## Import and generate

1. Choose a style and use an authenticated account/key with template-create permission.
2. Send its `.template.json` to `POST /api/v1/templates`. Do not import by copying only HTML: field definitions are also required. If the slug already exists, retrieve its ID and update that template through the normal template-update path; do not create duplicate slugs.
3. Fetch `GET /api/v1/templates/<slug>/schema` to confirm the imported fields and current version.
4. Replace illustrative article content and all `https://publisher.example/...` asset URLs in `.example.json`. `title` is the CMS content title; `data.headline` is the visible article title. Keep them aligned. The draft request uses `folder_path: /reports`; ensure this folder exists or select an existing destination folder.
5. Send the example request to `POST /api/v1/page-generation` with its default `mode: draft`. Creating a new page requires `content.create`; editing an existing page requires the corresponding edit permission and explicit upsert semantics. Use a unique slug for additional articles.
6. Review the generated draft/preview in the system. For a direct `mode: publish` request, add `expected_template_version` from the schema response and an `Idempotency-Key` header, with both create/edit and publish permissions as appropriate. Do not hard-code version 1. A template-version conflict requires refreshing the schema and validating the content again.
7. Publish explicitly. Changes to template HTML do not automatically replace already published pages; use the system's versioned upgrade/republish workflow.

No database records or live pages are created by merely adding these files to the repository. They still need to be imported through the system.

## Social sharing and production gate

Every layout server-renders canonical/`og:url` from `public_url`, article title/description, publisher, Open Graph article metadata, X/Twitter summary-card metadata, favicon and touch icon. Setting `share_image_url` emits both Open Graph and Twitter image tags. Omission intentionally emits no image tags rather than pretending a local relative image is production-ready.

Browser language switching does not alter crawler metadata: social cards use the English title/summary. If Chinese social cards are needed later, publish a separate Chinese URL with Chinese server-rendered metadata. Chat applications generally do not run this page's language script.

Complete [PROD-001](../../docs/TODO.md) before production deployment: real absolute HTTPS canonical/page/image/icon URLs, public HTTP 200 access and actual platform validation. Telegram, WhatsApp, LINE, LinkedIn and Facebook can use the supplied metadata, but their layouts, caches and user settings determine what they show. A favicon does not guarantee a small icon in every chat card. All eight styles are covered by this mandatory production checklist.

For a default social image, use `https://YOUR_PRODUCTION_DOMAIN/static/images/report-templates/<slug>-share.png`; never publish this literal placeholder. Provide an article-specific image of the same dimensions when desired. Further guidance: [SOCIAL-PREVIEW.md](SOCIAL-PREVIEW.md).

## Regeneration and local validation

The generator `build-report-templates.py` and shared `report-styles.css` are the sources of truth for the eight report layouts, schema payloads and illustrative examples. `crypto-styles.css` and `build-crypto-template.py` maintain the crypto layout and payload. The CSS is embedded during generation; published articles require no additional stylesheet request. Generators print an `apply_patch` change set; apply it to update files. They do not silently overwrite them. `build-report-templates.py <slug> --check` verifies generated report files are in sync. Regenerate after editing shared CSS or chrome. Do not leave `.html` and `.template.json` with different layouts.

Complete HTML documents are served directly, without the system theme wrapper or `/static/css/main.css`. HTML fragments still use the existing theme. This prevents theme `.hero` and `.sidebar` selectors from overriding report typography and layout. Authored title and JSON-LD metadata are preserved. All nine templates embed article JSON-LD using frozen renderer values.

The actual-page regression tool `verify-layout-repair.cjs` accepts `TEMPLATE_BASE_URL`, `TEMPLATE_SNAPSHOT` (an inventory containing template and content IDs), and `TEMPLATE_SCREENSHOT_DIR`. It checks all nine published pages at 1440/768/390/320px, spacing, overlap, headline contrast, print contrast, long body headings, wide tables, unbroken code, replacement characters and page errors. It reads published pages only; use the documented API to update templates and explicitly republish existing articles.

Generate local rendered examples with the real publication renderer:

```sh
GOCACHE="$PWD/bin/.go-build-cache" TMPDIR="$PWD/bin" go run ./examples/templates/render-preview
GOCACHE="$PWD/bin/.go-build-cache" TMPDIR="$PWD/bin" go test ./examples/templates/... -count=1
```

The local tool writes `bin/template-previews/` and does not touch a database or publish anything. Its URLs and HTTP assets are deliberately development-only. Open its generated HTML files, or serve the project temporarily on localhost and open [gallery.html](gallery.html). The gallery requires these generated preview files; raw `.html` templates contain placeholders and are not filled articles.

Browser checks use an installed Playwright package (set `PLAYWRIGHT_MODULE` to its module path if needed). They intercept local fixture requests, so no preview server is necessary:

```sh
TMPDIR="$PWD/bin" node examples/templates/verify-report-templates.cjs
```

Checks cover all eight styles: desktop English, mobile English (incl. zh-CN locale), unsupported-language English fallback, minimum loading delay with reduced motion, missing-Chinese-content fallback, server-rendered English metadata, unique table-of-contents links, copy feedback, no horizontal viewport overflow and no-JavaScript article access. Screenshots and the browser result file stay in ignored `bin/template-previews/`.

Assets can be regenerated with `generate-report-assets.py` using Python/Pillow; its font paths currently target macOS Arial. The committed PNG/SVG files are the deployment assets, so normal use requires neither Python nor Pillow.
