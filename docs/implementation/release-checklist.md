# Release Checklist — LightCMS 7.3.0 (V3 program)

Owner: Task 19 (release rehearsal + signoff). Evidence: `docs/implementation/release-report.md`.
Branch: `task/19-release`. Version: `build.json` → `7.3.0`, CHANGELOG entry added. Tag `v7.3.0` is cut LATER by the owner — this task does NOT tag.

## Preconditions (all must be checked with evidence pointers)

- [x] **Full regression green**: `go test -p 1 ./... -count=1` → exit 0, 27 packages ok, 0 FAIL (release-report §1; log `/tmp/19-final2.log`, verbose `/tmp/19-final-verbose.log`). **Re-validated 2026-10-02 after waves 1–4 + i18n merged (main `6e0d8d1`; later deltas are docs, example pages, and a `theme-vars.css` `--primary` default tweak that no test reads or embeds): exit 0, 29 packages ok, 0 FAIL, `go vet ./...` clean — log `/tmp/wave5-regression-20261002-020908.log`; scope note in release-report §11. Re-validated AGAIN 2026-10-02 after the R01–R12 + admin/API review-fix round (this tree): exit 0, 29 packages ok, 0 FAIL — log `/tmp/review-fixes-regression-20261002-190543.log`; scope note in release-report §12. Re-validated a THIRD time 2026-10-02 after the review-feedback round (fencing, gate hardening, table alignment, shadowing fix, pin updates — this tree): exit 0, 29 packages ok, 0 FAIL — log `/tmp/review-fixes-regression-20261002-225337.log`; scope note in release-report §13.**
- [x] **Vet + builds**: `go vet ./...` exit 0; `cmd/server`, `cmd/mcp`, `cmd/cli` all build (release-report §1).
- [x] **MCP inventory**: rebuilt `bin/lightcms-mcp` (gitignored, local only); runtime `tools/list` = **122 tools** incl. `get_template_schema` (release-report §2). Doc drift resolved 2026-10-02: `MCP.md` and `CLAUDE.md` both document 122 (wave 3C); rebuilt again 2026-10-02 on post-wave-4 main — count unchanged.
- [x] **Migration rehearsal** on representative fixtures (verified / mismatch / missing-static / deleted / fork / case-collision / empty-path): dry-run → blocked apply → admin repair → completed apply → scanner pass; no serving URL lost (release-report §3).
- [x] **Crash rehearsal**: real `kill -9` mid-publish AND mid-unpublish cutover (separate OS process, post-rename/pre-commit), same-data restart, scanner repair, outbox delivery (release-report §4).
- [x] **One binary / one UI**: single `lightcms` binary serves admin UI + API + MCP + public site (335 routes, one fly process group); only MongoDB + filesystem volume required (release-report §5).
- [x] **Gap triage a–g closed as fixed / workflow / accepted-risk** (release-report §6). No open BLOCKER from Task 19 scope except the external signoffs below.
- [x] **Version + changelog**: `build.json` 7.2.2 → 7.3.0, CHANGELOG `## [7.3.0]` entry (this commit); extended 2026-10-02 with the waves 1–4 / i18n / saveVersion / admin UX sections (everything unreleased since `v7.2.2` now sits in the 7.3.0 entry — `v7.3.0` still untagged).
- [x] **Tree hygiene**: no `.env.test` created/committed; no binaries committed (`bin/` gitignored); test-polluted `internal/handlers/static/sitemap.xml` restored; temp rehearsal harnesses retired after green (evidence retained in release-report).

## Release conditions (owner must clear before calling 7.3.0 fully-gated)

- [ ] **External signoff**: architecture, security, operations reviews recorded in release-report §9. Do NOT label production-ready before these checks.
- [ ] **CI restore**: `.github/` is absent in this tree — release cannot be called fully-gated without CI. Owner must restore CI (or confirm the canonical repo has it) and get a green CI run on the release commit.
- [ ] **Tag**: owner cuts `v7.3.0` AFTER signoffs; rebuild `bin/lightcms-mcp` from the tagged source per CLAUDE.md (binary encodes tool definitions).
- [ ] **Production migration drill**: dry-run `migrate-publications` against a COPY of production data; keep the report as the migration audit record.
- [ ] **Backup/restore drill**: per `docs/BACKUP-RESTORE.md` (volume snapshot + Mongo dump) before running apply on production.
- [ ] **Post-release follow-ups** (owners in release-report §6/§10): regenerate 410-vs-redirect decision, scheduler crash-lease TakeOver decision, MCP doc counts refresh.

## Deploy (production, `metavert-cms`)

- [ ] `./deploy.sh` (only supported path — see `docs/DEPLOYMENT-PRODUCTION.md`; legacy machine, ~8 min, no progress output).
- [ ] Verify `/health`, `/healthz`, admin version footer (7.3.0), `/llms.txt`.
- [ ] Run migration apply (after drill + backup); confirm flag `completed` and scanner clean pass.
