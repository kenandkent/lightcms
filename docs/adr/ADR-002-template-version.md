# ADR-002: Template Contract Versioning (immutable versions, explicit upgrade)

- **Status:** proposed
- **Date:** 2026-09-30
- **Task:** 0 (LightCMS V3 template static publishing program)
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`
  (§9.6 TemplateVersion, §39.4 Template Version Integration, §45.1)
- **Plan:** `docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md`
  (Tasks 2/3/4, Shared interface contract)

| Role | Approver | Decision |
|---|---|---|
| Tech lead | _(slot)_ | ☐ approved / ☐ changes requested |
| API owner (co-sign, schema endpoint + slug migration) | _(slot)_ | ☐ approved / ☐ changes requested |

## 1. Scope / Context

LightCMS v7 templates (`templates` collection, `models.Template` /
`models.TemplateField`) are mutable records: editing a template's HTML layout
or fields changes the meaning of all pages rendered from it, and today
`TemplateService.UpdateTemplate` re-renders live pages in place. There is no
immutable snapshot, no version pointer, and no way to answer "which template
revision produced this live HTML". Field types are limited to
`text, textarea, richtext, date, image, select` (CLAUDE.md); the program adds
`url, number, boolean` (+ existing `markdown/rawhtml` handling) under one
shared validator.

This ADR fixes the versioning boundary for Lane A (Tasks 3–4): slug identity,
canonical hashing, atomic version allocation, JSON Schema output, and the
explicit-upgrade rule.

## 2. Decision records

- **D1 — Slug is identity.** Template slug: lowercase kebab-case, immutable on
  ordinary update, unique via a `UNIQUE(slug)` index. Accepted:
  `financial-news`; rejected: uppercase, spaces, empty, >64 chars. Slug rename
  is an admin-only `POST /api/v1/templates/{id}/migrate-slug` operation that
  changes NO historical TemplateVersion, NO page URL, NO Publication, acquires
  NO page-path locks (plan Task 12).
- **D2 — Immutable versions.** Each template has `CurrentVersion int64`;
  each `TemplateVersion` is an immutable document carrying the full field
  snapshot (name, category, status, HTML layout, script policy, field
  definitions incl. description/example/validation). Readers (renderer, schema
  endpoint, validator) ALWAYS read the immutable version, never the mutable
  Template record.
- **D3 — ContractHash vs RenderHash.** `ContractHash` = canonical JSON hash
  over everything affecting validation/rendering; equal hash ⇒ no new version.
  `RenderHash` = hash over rendering-affecting subset only, so a
  description-only change bumps the version but keeps `RenderHash` stable
  (lets Task 14 skip byte-identical re-renders).
- **D4 — Atomic allocation.** Version creation runs in one Mongo transaction:
  CAS-increment `templates.current_version` (expected-version match; loser gets
  `TEMPLATE_VERSION_CONFLICT`, no duplicate version document) + insert
  immutable version under `UNIQUE(template_id, version)` (index defined in
  Task 2, created post-migration in Task 14).
- **D5 — One validator, deterministic schema.** `ValidateData(v
  TemplateVersion, data map[string]any)` and `ToJSONSchema(v TemplateVersion)`
  (plan Shared interface contract) are the single implementation shared by
  Admin and API. Schema output for the same immutable version is byte-for-byte
  stable, carries `additionalProperties:false`, `format:date`, enums, defaults,
  descriptions, examples; schema HTTP responses carry an ETag bound to the same
  version.
- **D6 — Explicit upgrade only.** Template HTML/field changes create a new
  TemplateVersion and NEVER regenerate live pages. Moving a page to a newer
  template version requires the explicit Upgrade Preview (read-only diff) +
  durable Upgrade Job path (per-content progress, retry/resume); publish after
  a template increment without `expected_template_version` refresh returns
  `409 TEMPLATE_VERSION_CHANGED` with zero mutation.

## 3. Rejected alternatives

- **A1 — Mutable template with "last render wins".** Rejected: live HTML
  becomes unreproducible; rollback (§39.11) cannot guarantee byte-identical
  restores; concurrent template edit + publish races are unresolvable.
- **A2 — Version on every save regardless of content.** Rejected: version
  spam breaks the "same ContractHash ⇒ no new version" idempotency Tasks 11/12
  rely on, and pollutes upgrade-preview diffs.
- **A3 — Schema generated from the live Template record.** Rejected: schema
  drifts underfoot while agents paginate/integrate; violates the byte-stability
  requirement agents and OpenAPI consumers need.

## 4. Consequences / risks

- Storage: one immutable document per effective template change; bounded and
  tiny relative to page volume. No GC of versions referenced by retained
  Publications (Task 10 pins them).
- Historical schema drift: pre-V3 templates may carry slugs/fields that fail
  new validation; Task 14 dry-run reports them without writing data, and
  remediation precedes G1 index creation.
- API surface growth (schema endpoint, migrate-slug, upgrade preview/job) is
  owned by Lane E (Task 12); this ADR constrains their semantics, not their
  routes.

## 5. Crash / failure / concurrency behavior

- Two concurrent updates from the same expected version: exactly one commits
  N+1, the other receives `TEMPLATE_VERSION_CONFLICT`; the unique
  `(template_id, version)` index makes double-insert impossible even under
  retry.
- Crash between CAS-increment and version insert aborts the transaction; no
  partial version is visible. Retry re-reads current version and proceeds.

## 6. Migration & rollback

- Migrate: Task 14 backfills `CurrentVersion=1` + initial immutable versions
  from existing templates; invalid/duplicate slugs block `completed`
  (reported, not auto-fixed).
- Rollback: versions are additive; rolling back the contract code leaves inert
  version documents. Live behavior rollback follows ADR-001 (16B revert).

## 7. Verification (commands + expected evidence)

```bash
go test -p 1 ./internal/product/templatecontract -run 'TestSlug|TestVersion' -count=1
# Red before Task 3 (package does not exist); green after: slug table,
# ContractHash no-op, RenderHash stability, concurrent-update conflict.
go test ./internal/product/templatecontract -run 'TestValidateData|TestJSONSchema' -count=1
# Red before Task 4; green after, incl. date=2026-02-30 rejection and
# byte-stable schema assertions.
```

## 8. Approval record

_To be filled at G1 review. Unapproved ADRs MUST NOT be handed to parallel
developers for independent interpretation (spec §42)._
