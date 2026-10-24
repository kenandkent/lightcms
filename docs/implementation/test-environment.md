# Test Environment (Task 0 — reproducible MongoDB replica set)

- **Baseline SHA:** `29a7276` (branch `task/0-test-env-adr`, based on `main`)
- **Date:** 2026-09-30
- **MongoDB image (pinned):** `mongo:7.0.14`
  (`sha256:0032d2ca20db5fa34926f196c8a43b74e34ed239a7f2453ff1505b6f12ba8ea6` —
  resolved at pull time; re-pin the digest here if the image is re-pulled)
- **Verified runtime:** `mongosh --eval db.version()` → `7.0.14`,
  `rs.status()` → single member `PRIMARY` (myState=1)

## 1. Why a replica set

V3 needs multi-document transactions (`DB.WithTransaction`, Task 2;
publication activation, unpublish, idempotency binding, outbox insert).
Standalone `mongod` CANNOT run transactions. A single-node replica set (`rs0`)
is the minimum transaction-capable topology for local dev and CI.

## 2. Bring-up (local)

```bash
# 1. Start Docker (daemon must be running), then:
docker compose -f docker-compose.test.yml up -d

# 2. Confirm the RS is initialized and PRIMARY:
docker compose -f docker-compose.test.yml logs mongo-init
# expected: "RS initiate issued" (first run) then "RS ready (myState=1)";
# on restart: "RS already initialized" then "RS ready (myState=1)".
docker exec lightcms-mongo-test mongosh --quiet --eval 'db.version(); rs.status().myState'
# expected: 7.0.14 / 1

# 3. Configure the test URI (gitignored local file — never commit):
cp .env.test.example .env.test

# 4. Run DB-backed tests (ALWAYS -p 1 for DB packages):
go test -p 1 ./internal/database/ -count=1
go test -p 1 ./internal/services/ -count=1 -run 'TestGetContent$'

# 5. Teardown (keep volume) / full wipe (drop test data):
docker compose -f docker-compose.test.yml down
docker compose -f docker-compose.test.yml down -v
```

Readiness checklist before claiming integration evidence: `mongo-init` logged
`RS ready`, `myState=1`, and a transaction smoke test commits (verified
2026-09-30 via mongosh `startSession/startTransaction/commitTransaction`
round-trip against `lightcms-test`).

## 3. Test commands (unit vs integration gate)

```bash
# Fast unit gate — DB skip allowed (no MONGODB_URI needed):
go build ./... && go vet ./...

# Integration gate — replica-set URI REQUIRED. Fails when absent:
: "${MONGODB_URI:?MONGODB_URI (or .env.test) must point at the test replica set — see §2}"
go test -p 1 ./... -count=1
```

Rule (plan Global Constraints): tests that require Mongo MUST fail the release
gate when no test Mongo URI is configured; a green run with DB tests skipped
is NOT successful integration evidence and does not satisfy G2/G3/G4. Until CI
is restored (§5), enforce this by exporting `MONGODB_URI` (or keeping
`.env.test`) and grepping the run for `skipping: MONGODB_URI not set` — any
such line invalidates the run as integration evidence.

## 4. Safety guard (verified, read-only)

`internal/testutil/testutil.go` → `MustConnectTestDB` refuses any
`DATABASE_NAME` not containing `test` (case-insensitive):

> `REFUSING to run tests — database name ... does not contain 'test'.`

Verified 2026-09-30 (no source changes — env override only):

- `DATABASE_NAME=lightcms-prod … go test -p 1 ./internal/services/ -run
  'TestGetContent$'` → `FAIL` with the `REFUSING` message. Guard works.
- **Known gap (recorded, NOT fixed in Task 0 — Go-source change owned by
  Task 2):** `internal/database` package tests use a local `testDB` helper
  (`internal/database/mongo_test.go:17`, kept local to avoid an import cycle)
  that does NOT enforce the `test` guard — a `DATABASE_NAME=lightcms-prod`
  run against that package connects instead of refusing. Task 2 (owner of
  `internal/database/mongo.go` + `internal/testutil/testutil.go`) should add
  the same guard to the local helper or route it through testutil.

## 5. CI-workflow decision record (binding)

**Fact:** `.github/workflows/` does not exist in this worktree. Commit
`29a7276` ("chore: remove GitHub workflows (ci, publish-mcp-image)") —
authored by the repo owner — deliberately deleted `ci.yml` (build + Atlas-backed
test + Codecov upload) and `publish-mcp-image.yml`. The tree was otherwise
clean at Task 0 start (`git status --short --branch` empty, no untracked
files).

**Decision:** leave CI absent. Task 0 does NOT recreate or restore either
workflow, per the plan ("Do not silently restore or overwrite a user-deleted
workflow") and the task brief. Consequences:

- There is currently NO automated build/test/release chain on this branch.
- The deleted `ci.yml` used Atlas (`secrets.MONGODB_URI`,
  `DATABASE_NAME=lightcms-test-<run_id>`, `go test ./... -p 1`, Codecov).
  Its replacement must use a replica-set-capable Mongo (service container with
  `--replSet`, or Atlas) and MUST implement the §3 integration gate
  (fail on missing URI / skipped DB tests).
- **CI must be re-added by the repo owner before G4** (Task 17/19 evidence
  depends on it). Re-adding CI is explicitly out of scope for Task 0 workers.

## 6. Task 0 verification evidence (2026-09-30, this worktree)

| Check | Command | Result |
|---|---|---|
| Build | `go build ./...` | exit 0 |
| Vet | `go vet ./...` | exit 0 |
| RS bring-up | `docker compose -f docker-compose.test.yml up -d` | `mongo-init`: `RS ready (myState=1)` (after fixing `--replset` → `--replSet` in the compose file) |
| Txn proof | mongosh session txn round-trip | `TXN_COMMIT_OK count=1` |
| DB package | `go test -p 1 ./internal/database/ -count=1` | `ok … 0.679s` (Mongo 7.0.14) |
| testutil path | `go test -p 1 ./internal/services/ -run 'TestGetContent$'` | `--- PASS: TestGetContent (0.08s)` |
| Guard negative | `DATABASE_NAME=lightcms-prod … -run 'TestGetContent$'` (no `.env.test`) | `FAIL` with `REFUSING…` message |
| Docker note | daemon was stopped at task start; started via Docker Desktop before bring-up | blocker cleared, no file impact |

`.env.test` used for the runs above was a local, gitignored copy of
`.env.test.example`; the committed tree contains only the `.example` file.
Stack torn down with `docker compose -f docker-compose.test.yml down -v`
after verification.

## 7. Baseline / plan reconciliation notes

- Plan Global Constraints name baseline `c1165be` (v7.2.2) and instruct
  recording a new baseline before rebasing: Task 0 baseline is `29a7276`
  (= `c1165be` + design/plan docs commit `e01c1cc` + workflow-removal commit
  `29a7276`). No code delta vs `c1165be` outside those two commits.
- No spec↔plan conflicts found within Task 0 scope (spec §42 ADR contents ⊇
  plan Task 0 file list; both require five ADRs in `docs/adr/`, replica-set
  fixture, `test`-named DB, `-p 1` DB runs, and no silent CI restoration).
