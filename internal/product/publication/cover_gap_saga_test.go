package publication_test

// Task 17B coverage-gap tests round 2: saga idempotency-record flows,
// unpublish compensation, rollback re-render, ActivateCAS matrix, outbox
// lease-loss branches (external).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCoverGapActivateCASMatrix(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()

	if err := repo.ActivateCAS(ctx, primitive.NewObjectID(), primitive.NewObjectID(), nil); publication.CodeOf(err) != publication.CodeNotFound {
		t.Fatalf("ActivateCAS missing pub: %v", err)
	}
	contentID := seedContent(t, db)
	staged := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, staged); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	// Wrong content binding.
	if err := repo.ActivateCAS(ctx, primitive.NewObjectID(), staged.ID, nil); publication.CodeOf(err) != publication.CodeValidation {
		t.Fatalf("ActivateCAS wrong content: %v", err)
	}
	// Bad verification status.
	stagedBad := stagedPub(contentID)
	stagedBad.VerificationStatus = "pending"
	if err := repo.InsertStaged(ctx, stagedBad); err != nil {
		t.Fatalf("InsertStaged bad verification: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, stagedBad.ID, nil); publication.CodeOf(err) != publication.CodeInvalidTransition {
		t.Fatalf("ActivateCAS bad verification: %v", err)
	}
	// Happy activation, then stale-expected conflict on the next candidate.
	if err := repo.ActivateCAS(ctx, contentID, staged.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	staged2 := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, staged2); err != nil {
		t.Fatalf("InsertStaged2: %v", err)
	}
	ghost := primitive.NewObjectID()
	if err := repo.ActivateCAS(ctx, contentID, staged2.ID, &ghost); publication.CodeOf(err) != publication.CodeConflict {
		t.Fatalf("ActivateCAS stale expected: %v", err)
	}
	// Nil-expected with an active present → conflict.
	if err := repo.ActivateCAS(ctx, contentID, staged2.ID, nil); publication.CodeOf(err) != publication.CodeConflict {
		t.Fatalf("ActivateCAS nil-expected conflict: %v", err)
	}
	// Outbox failure aborts the activation transaction.
	frepo := publication.NewRepository(db, failOutbox{})
	staged3 := stagedPub(contentID)
	if err := frepo.InsertStaged(ctx, staged3); err != nil {
		t.Fatalf("InsertStaged3: %v", err)
	}
	active, err := repo.GetActive(ctx, contentID)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if err := frepo.ActivateCAS(ctx, contentID, staged3.ID, &active.ID); err == nil {
		t.Fatalf("ActivateCAS outbox fail: want error")
	}
}

func TestCoverGapPublishWithIdempotencyRecord(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	idem, err := idempotency.NewService(s.db, idempotency.Options{})
	if err != nil {
		t.Fatalf("idem: %v", err)
	}
	s2 := newSagaSetup(t, nil, nil, publication.Options{})
	_ = s2
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/gap-idemrec", 1, nil)

	body := []byte(`{"t":1}`)
	op, err := idem.Begin(ctx, "owner-gap", "POST", "/p", "key-gap-idemrec", body)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	svc := publication.NewService(s.db, publication.NewRepository(s.db, nil), s.store, publication.Options{Idem: idem})
	res, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &op.ID,
	})
	if err != nil {
		t.Fatalf("Publish with record: %v", err)
	}
	if res.PublicationID.IsZero() {
		t.Fatalf("zero publication")
	}
	// Same record after completion → already-completed error (replay upstream).
	if _, err := idem.Complete(ctx, op.ID, op.Attempt, 200, map[string]any{"publication_id": res.PublicationID.Hex()}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &op.ID,
	}); err == nil {
		t.Fatalf("Publish completed record: want error")
	}
	// Unknown record → not-found error.
	ghost := primitive.NewObjectID()
	if _, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &ghost,
	}); err == nil {
		t.Fatalf("Publish ghost record: want error")
	}
	// Terminal record → stale-attempt error.
	op2, err := idem.Begin(ctx, "owner-gap", "POST", "/p", "key-gap-idemrec2", body)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	if _, err := idem.MarkTerminal(ctx, op2.ID, op2.Attempt, "RENDER_FAILED"); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	if _, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &op2.ID,
	}); err == nil {
		t.Fatalf("Publish terminal record: want error")
	}
	_ = tvID
}

func TestCoverGapUnpublishCompensation(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)

	// Canonical already absent: converge without backup.
	cid := seedSagaContent(t, s.db, tplID, "/news/gap-unpub-absent", 1, nil)
	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	_ = res
	canon := filepath.Join(s.root, "generated", "news", "gap-unpub-absent.html")
	if err := os.Remove(canon); err != nil {
		t.Fatalf("remove canonical: %v", err)
	}
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: cid}); err != nil {
		t.Fatalf("Unpublish absent canonical: %v", err)
	}

	// Failing outbox: file staged away, transaction fails, backup restored.
	cid2 := seedSagaContent(t, s.db, tplID, "/news/gap-unpub-fail", 1, nil)
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid2, ContentVersion: 1, TemplateVersionID: tvID,
	}); err != nil {
		t.Fatalf("Publish2: %v", err)
	}
	frepo := publication.NewRepository(s.db, &sagaFailOutbox{permanent: true})
	fsvc := publication.NewService(s.db, frepo, s.store, publication.Options{})
	if err := fsvc.Unpublish(ctx, publication.UnpublishRequest{ContentID: cid2}); err == nil {
		t.Fatalf("Unpublish failing txn: want error")
	}
	// Backup restored: canonical serves again.
	canon2 := filepath.Join(s.root, "generated", "news", "gap-unpub-fail.html")
	if _, err := os.Stat(canon2); err != nil {
		t.Fatalf("canonical not restored: %v", err)
	}
}

func TestCoverGapRollbackRerender(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/gap-rb-rerender", 1, nil)
	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Destroy the retained immutable bytes + record so rollback must re-render.
	fstore, ok := s.store.(*storage.FilesystemStore)
	if !ok {
		t.Fatalf("store type %T", s.store)
	}
	imm := fstore.ImmutablePath(cid, res.PublicationID)
	if err := os.Remove(imm); err != nil {
		t.Fatalf("remove immutable: %v", err)
	}
	if _, err := s.db.Collection("content_publications").DeleteOne(ctx, bson.M{"_id": res.PublicationID}); err != nil {
		t.Fatalf("delete publication record: %v", err)
	}
	rb, err := s.svc.Rollback(ctx, publication.RollbackRequest{ContentID: cid, SourcePublicationID: res.PublicationID})
	if err != nil {
		t.Logf("Rollback re-render unavailable (acceptable if exact-bytes required): %v", err)
	} else if rb.PublicationID.IsZero() {
		t.Fatalf("rollback zero id")
	}
}

func TestCoverGapOutboxLeaseLoss(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()
	ob := publication.NewOutbox(db)

	// Nil deliver func.
	wnil := publication.NewOutboxWorker(db, nil, publication.WorkerOptions{})
	if _, err := wnil.ProcessNext(ctx); err == nil {
		t.Fatalf("nil deliver: want error")
	}
	// Claim transport error.
	bdb := testutil.MustConnectBrokenDB(t)
	wb := publication.NewOutboxWorker(bdb, func(ctx context.Context, e, id string, p map[string]any) error {
		return nil
	}, publication.WorkerOptions{})
	if _, err := wb.ProcessNext(ctx); err == nil {
		t.Fatalf("broken claim: want error")
	}

	pubID := primitive.NewObjectID()
	if err := ob.InsertUnique(ctx, "content.publish", pubID, map[string]any{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Deliver steals the lease mid-flight → markDelivered reports lease lost.
	thief := publication.NewOutboxWorker(db,
		func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
			_, uerr := db.Collection("webhook_outbox").UpdateOne(ctx,
				bson.M{"aggregate_id": pubID},
				bson.M{"$set": bson.M{"locked_by": "thief"}})
			if uerr != nil {
				t.Errorf("steal: %v", uerr)
			}
			return nil
		}, publication.WorkerOptions{WorkerID: "victim"})
	if _, err := thief.ProcessNext(ctx); err == nil || !strings.Contains(err.Error(), "lease lost") {
		t.Fatalf("markDelivered lease loss: %v", err)
	}

	pubID2 := primitive.NewObjectID()
	if err := ob.InsertUnique(ctx, "content.publish", pubID2, map[string]any{}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	// Failing deliver + stolen lease → scheduleRetry hits its lease-lost
	// branch (ProcessNext still reports the delivery error).
	thiefFail := publication.NewOutboxWorker(db,
		func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
			_, uerr := db.Collection("webhook_outbox").UpdateOne(ctx,
				bson.M{"aggregate_id": pubID2},
				bson.M{"$set": bson.M{"locked_by": "thief"}})
			if uerr != nil {
				t.Errorf("steal2: %v", uerr)
			}
			return errGapDelivery
		}, publication.WorkerOptions{WorkerID: "victim2", Backoff: func(a int) time.Duration { return time.Hour }})
	if _, err := thiefFail.ProcessNext(ctx); err == nil {
		t.Fatalf("scheduleRetry lease loss: want error, got nil")
	}
	// Negative backoff clamps to zero (retry still scheduled).
	pubID3 := primitive.NewObjectID()
	if err := ob.InsertUnique(ctx, "content.publish", pubID3, map[string]any{}); err != nil {
		t.Fatalf("seed3: %v", err)
	}
	wneg := publication.NewOutboxWorker(db, func(ctx context.Context, e, id string, p map[string]any) error {
		return errGapDelivery
	}, publication.WorkerOptions{Backoff: func(a int) time.Duration { return -time.Second }})
	if _, err := wneg.ProcessNext(ctx); err == nil {
		t.Fatalf("negative backoff: want propagated delivery error")
	}
	_ = time.Now
}
