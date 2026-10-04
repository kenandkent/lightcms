# Social link previews

Production requirement: complete [PROD-001](../../docs/TODO.md) after selecting the production domain. This applies to every public-page template in the project and is a mandatory go-live gate.

The template includes server-rendered Open Graph metadata, X/Twitter summary cards, a favicon, and an Apple touch icon. Preview text comes from the article fields rather than browser-language JavaScript.

The eight additional report/announcement layouts use the same production gate. Their shared field contract, import payloads and style-specific images are documented in [REPORT-TEMPLATES.md](REPORT-TEMPLATES.md). Their optional Chinese article fields affect browser content only; social metadata remains server-rendered English.

## Template configuration

Keep the existing headline, summary, author and category fields. Add these fields to the LightCMS Template before sending them in generation requests:

| Field | Type | Required | Value |
| --- | --- | --- | --- |
| share_image_url | url | Recommended for image previews | Public absolute HTTPS URL of a 1200×630 PNG or JPEG |
| share_image_alt | text | No | Description of the preview image; defaults to Chain Lens crypto market analysis |
| favicon_url | url | No | Public icon URL; otherwise uses /static/images/chain-lens-icon.svg |

Use AllowedProtocols=["https"] for the URL fields. `public_url` and `published_at` are system values; do not add them to user data. If `share_image_url` is omitted, the template emits title and description metadata but no preview image. Only use the declared 1200×630 dimensions with an image of that size.

The default assets are served by the existing LightCMS `/static/` route:

- `/static/images/chain-lens-share.png` — 1200×630 link-preview image.
- `/static/images/chain-lens-icon.svg` — browser favicon.
- `/static/images/chain-lens-touch.png` — 180×180 Apple touch icon.

Once the public domain is chosen, set BASE_URL/PUBLIC_BASE_URL appropriately and supply share_image_url with that domain plus the default image path, or use an uploaded article-specific image. A domain is not required to edit or preview the template locally.

## Publishing the standalone AGT page

The example AGT HTML has populated English title and description metadata. Its default image paths remain root-relative because no public domain has been selected. Before social sharing, add `<link rel="canonical">` and `og:url` with the actual article URL, and replace both og:image and twitter:image with the absolute HTTPS image URL. Root-relative paths are local/deployment preparation, not a guarantee of crawler compatibility.

## Validation after deployment

The page and image must be publicly fetchable without login, cookies or browser JavaScript. Check HTTP 200 responses, correct image Content-Type and valid HTTPS. Verify that bot access is permitted by the site's robots/WAF rules. The loading animation does not defer metadata or article delivery.

Use Facebook Sharing Debugger and LinkedIn Post Inspector to refresh cached metadata; also share the public URL in Telegram, WhatsApp and LINE to inspect their actual cards. Client settings, caches and platform layout determine which fields appear. A favicon does not guarantee a small icon in every chat preview; og:image is the primary preview artwork.

Sources: [Open Graph protocol](https://ogp.me/), [LinkedIn sharing requirements](https://www.linkedin.com/help/learning/answer/a521928), [LinkedIn Post Inspector](https://www.linkedin.com/help/linkedin/answer/a6269011).
