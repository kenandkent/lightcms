# Cloudflare version-upload diagnosis

## Reproduction

The blocked job `6ad77512e5aefb0533504321` references frozen release
`6ad77505e5aefb0533504310`. The diagnostic command uses the project's
`ReleaseUploadFiles`, `ReleaseModules`, `cloudflare.UploadAssets`, and
`cloudflare.UploadVersion` implementations. Credentials are supplied through
stdin, never command-line arguments or log output.

The dedicated target is `lightcms-reports-test`. No activation API is called.
The probe compares deployments before and after the upload attempt.

Server-side execution on 2026-10-20 produced:

```text
probe: 4 assets, 4 modules, mode=assets-first-v1; no activation
asset upload succeeded
HTTP 400, Cloudflare error 10162:
module delivery-assets-config.json has unsupported Content-Type application/json
active deployments unchanged
```

CF-Ray: `a4d8bfc9dff52bc5-YUL`.

This proves a real upload-protocol incompatibility, not an asset-export failure.
The existing `routes.json` module also uses application/json; its compatibility
must be addressed and verified when fixing the module packaging. Do not assume
that correcting only the first named module completes the fix. Local handler
tests do not validate Cloudflare's multipart module parser.

## Diagnostic changes

- Preserve HTTP status, request method/path (without query), CF-Ray, and up to
  five bounded Cloudflare error codes/messages.
- Redact the account credential, upload credential and long token-like strings;
  strip control characters. Never retain raw bodies or authorization headers.
- Log the sanitized diagnostic with the job ID, stage and application error code.
- Keep the job-state/activation behavior unchanged.

The original blocked job has not been cancelled, retried, or modified. The probe
uploaded static assets, but the version upload was rejected and nothing was
activated. Diagnostic changes are local and require deployment to produce new
production logs. The upload packaging itself is not fixed by this logging change.

## Packaging correction and real API verification

The follow-up correction consumes `delivery-assets-config.json` only as upload
metadata, omitting it from multipart Worker modules. `routes.json` is uploaded as
a supported `text/plain` module; the entrypoint parses its imported string with
`JSON.parse`. Other application/json Worker modules are rejected locally.

Regression tests first failed against the previous packaging. After correction,
the same server-side probe, same frozen release, and same dedicated Worker produced:

```text
asset upload succeeded
active deployments unchanged
version upload succeeded: 512de019-8062-45c2-ab50-80fb41a7e9d7 (not activated)
```

This verifies the actual Cloudflare multipart parser and module compilation,
not only a mocked response. No activation was performed and the original blocked
job remains untouched. The new version is an inactive diagnostic version.
