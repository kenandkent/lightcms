# Assets-only Cloudflare delivery

Approved scope: remove JavaScript from both public Workers, preserve existing HTML,
domains, ownership checks, publication recovery and CMS deployment behavior.

Implementation sequence:

1. Add regression tests for metadata-only uploads and native health JSON assets.
2. Introduce a versioned assets-only release format. Keep old frozen releases
   readable; newly built releases use only native assets and native routing.
3. Store compact ownership identity in Cloudflare version annotations, not runtime
   bindings. Inspect the version identifier for drift detection when no code ETag
   exists. Serve the existing health contract as a no-store JSON asset.
4. Initialize new dedicated targets with assets only. Preserve the existing
   uncertain-initialization and ownership safeguards.
5. Run package tests, database integration tests, build and vet. Verify the real
   Cloudflare API accepts metadata-only uploads and retains ownership annotations.
6. Migrate the existing empty report target with precondition checks and rollback
   information; do not change articles or domain mappings. Verify both Workers
   have no script handlers and unknown URLs produce native 404 responses.

Constraints: main only, no subagents, no changes to umx-home.html. Never print
credentials. Do not claim production completion until the deployed CMS and remote
Worker state have both been verified.
