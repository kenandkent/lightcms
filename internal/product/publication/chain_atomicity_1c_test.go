package publication_test

// Lane 1C (chain atomicity) red-green tests.
//
// Scope: spec §§15/16/21/28 govern (no spec conflict found — §28.1 mandates
// the outbox row be written inside the activation transaction with
// UNIQUE(event_type, aggregate_id) as the idempotency arbiter, and shows the
// content.publish payload carrying public_url at commit time).
//   - Fix 1: mongoOutbox.InsertUnique duplicate = silent success (unified with
//     publication Outbox.InsertUnique). Replay-after-commit keeps the live
//     canonical + a single outbox row.
//   - Fix 2: the committed outbox payload always carries public_url at
//     transaction time (no post-commit enrichment race); the worker-delivered
//     payload carries it too.
//
// DB note: these tests REQUIRE the Task 0 replica-set fixture
// (MONGODB_URI + test-named DATABASE_NAME); skips are not green evidence.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
)

// TestChainAtomicity_ReplayAfterCommitKeepsCanonical: a full saga publish cuts
// the live canonical and commits exactly one outbox row; an idempotent replay
// of the committed activation returns nil, leaves the canonical bytes
// untouched, and inserts no second row.
func TestChainAtomicity_ReplayAfterCommitKeepsCanonical(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/chain-replay", 1, nil)

	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	liveBefore := readSagaCanonical(t, s.root, "/news/chain-replay")

	// Idempotent replay of the committed activation (lost-response retry).
	if err := s.repo.ActivateCAS(ctx, contentID, res.PublicationID, nil); err != nil {
		t.Fatalf("replay ActivateCAS: %v", err)
	}
	if liveAfter := readSagaCanonical(t, s.root, "/news/chain-replay"); liveAfter != liveBefore {
		t.Fatalf("replay changed the live canonical:\nbefore: %s\nafter: %s", liveBefore, liveAfter)
	}
	if n := sagaOutboxCount(t, s.db, publication.EventPublished, res.PublicationID); n != 1 {
		t.Fatalf("replay outbox rows = %d, want exactly 1", n)
	}
	active, err := s.repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != res.PublicationID {
		t.Fatalf("active = (%v, %v), want %s", active, err, res.PublicationID.Hex())
	}
}

// TestChainAtomicity_OutboxDuplicateIsSilentSuccess: when the outbox row for
// a publication already exists (committed by an earlier attempt whose
// response was lost), ActivateCAS must still commit the activation instead of
// aborting with PUBLICATION_CONFLICT. Aborting here is what used to push the
// saga into failCommitted compensation, which can delete a just-cut live
// canonical whose bytes match the replayed attempt.
func TestChainAtomicity_OutboxDuplicateIsSilentSuccess(t *testing.T) {
	db, repo := testRepo(t) // default mongoOutbox inserter
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}

	// Simulate the earlier committed attempt: the outbox row already exists.
	pre := publication.NewOutbox(db)
	if err := pre.InsertUnique(ctx, publication.EventPublished, a.ID,
		map[string]any{"content_id": contentID.Hex()}); err != nil {
		t.Fatalf("pre-insert outbox: %v", err)
	}

	// The activation must succeed (duplicate = silent success), not abort.
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS with pre-existing outbox row: %v (want nil — duplicate is silent success)", err)
	}
	if n := outboxCount(t, db, publication.EventPublished, a.ID); n != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1 (no second row)", n)
	}
	active, err := repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != a.ID {
		t.Fatalf("active = (%v, %v), want %s", active, err, a.ID.Hex())
	}
	if doc := getContent(t, db, contentID); doc["published"] != true {
		t.Fatalf("Content.Published = %v, want true", doc["published"])
	}
}

// TestChainAtomicity_DeliveredPayloadCarriesPublicURL: end to end, the payload
// the outbox worker delivers for a saga-published page always carries
// public_url — it is committed in the activation transaction (spec §28
// payload shape), so delivery can never observe it missing no matter when
// the worker runs relative to post-commit bookkeeping.
func TestChainAtomicity_DeliveredPayloadCarriesPublicURL(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/chain-url", 1, nil)

	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.PublicURL == "" || !strings.Contains(res.PublicURL, "/news/chain-url") {
		t.Fatalf("result public URL = %q, want it to contain /news/chain-url", res.PublicURL)
	}

	// The committed row carries public_url at transaction time (no join).
	var row bson.M
	if err := s.db.Collection("webhook_outbox").FindOne(ctx, bson.M{
		"event_type": publication.EventPublished, "aggregate_id": res.PublicationID,
	}).Decode(&row); err != nil {
		t.Fatalf("load outbox row: %v", err)
	}
	payload, _ := row["payload"].(bson.M)
	if payload == nil || payload["public_url"] != res.PublicURL {
		t.Fatalf("committed payload public_url = %v, want %q", payload, res.PublicURL)
	}

	// And the worker delivers exactly that payload.
	var delivered map[string]any
	worker := publication.NewOutboxWorker(s.db,
		func(_ context.Context, _ string, _ string, p map[string]any) error {
			delivered = p
			return nil
		},
		publication.WorkerOptions{WorkerID: "chain-1c-url-worker", Lease: time.Minute, Backoff: shortBackoff},
	)
	didWork, err := worker.ProcessNext(ctx)
	if err != nil || !didWork {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", didWork, err)
	}
	if delivered == nil || delivered["public_url"] != res.PublicURL {
		t.Fatalf("delivered payload public_url = %v, want %q", delivered, res.PublicURL)
	}
}
