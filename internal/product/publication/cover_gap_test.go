package publication_test

// Task 17B coverage-gap tests for internal/product/publication (external).

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func gapSnapshotContent() models.Content {
	return models.Content{
		ID:           primitive.NewObjectID(),
		Title:        "T",
		FullPath:     "/news/gap",
		Data:         map[string]any{"headline": "h"},
	}
}

func gapSnapshotTemplate() templatecontract.TemplateVersion {
	return templatecontract.TemplateVersion{
		ID:         primitive.NewObjectID(),
		TemplateID: primitive.NewObjectID(),
		Version:    2,
		Slug:       "news",
		HTMLLayout: "<p>x</p>",
	}
}

func TestCoverGapPlanSnapshotMatrix(t *testing.T) {
	content := gapSnapshotContent()
	tv := gapSnapshotTemplate()
	pubID := primitive.NewObjectID()
	now := time.Now()
	snap, err := publication.PlanSnapshot(content, 1, tv, pubID, now, "https://example.com/news/gap",
		publication.PlanOptions{
			ScriptPolicy: "admin_only", AuthorIsAdmin: true,
			DependencySnapshot: map[string]any{"theme": "a"},
			Snippets:           map[string]string{"s": "b"},
			TitleToPath:        map[string]string{"t": "/p"},
			PathToTitle:        map[string]string{"/p": "t"},
			LCQueryCache:       map[string]string{"q": "h"},
		})
	if err != nil {
		t.Fatalf("PlanSnapshot: %v", err)
	}
	if snap.ScriptPolicy != "admin_only" || !snap.AuthorIsAdmin || snap.DependencySnapshot["theme"] != "a" {
		t.Fatalf("snapshot fields: %+v", snap)
	}
	// Inherit policy resolves to "all".
	inh, err := publication.PlanSnapshot(content, 1, tv, pubID, now, "https://example.com/x", publication.PlanOptions{})
	if err != nil || inh.ScriptPolicy != "all" {
		t.Fatalf("inherit policy: %+v %v", inh, err)
	}
	// Unknown explicit policy is rejected.
	if _, err := publication.PlanSnapshot(content, 1, tv, pubID, now, "https://example.com/x",
		publication.PlanOptions{ScriptPolicy: "bogus"}); err == nil {
		t.Fatalf("unknown policy: want error")
	}
	// Every required-field validation.
	badContent := content
	mutators := []func(*models.Content, *templatecontract.TemplateVersion, *primitive.ObjectID, *time.Time, *string){
		func(c *models.Content, _ *templatecontract.TemplateVersion, p *primitive.ObjectID, _ *time.Time, _ *string) {
			*p = primitive.NilObjectID
		},
		func(c *models.Content, _ *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, _ *string) {
			c.ID = primitive.NilObjectID
		},
		func(c *models.Content, _ *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, _ *string) {
			c.FullPath = "  "
		},
		func(c *models.Content, tv *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, _ *string) {
			tv.ID = primitive.NilObjectID
		},
		func(c *models.Content, tv *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, _ *string) {
			tv.Version = 0
		},
		func(c *models.Content, tv *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, _ *string) {
			tv.HTMLLayout = ""
		},
		func(c *models.Content, _ *templatecontract.TemplateVersion, _ *primitive.ObjectID, t *time.Time, _ *string) {
			*t = time.Time{}
		},
		func(c *models.Content, _ *templatecontract.TemplateVersion, _ *primitive.ObjectID, _ *time.Time, u *string) {
			*u = ""
		},
	}
	_ = badContent
	for i, mut := range mutators {
		c := gapSnapshotContent()
		v := gapSnapshotTemplate()
		p := pubID
		lt := now
		u := "https://example.com/x"
		mut(&c, &v, &p, &lt, &u)
		if _, err := publication.PlanSnapshot(c, 1, v, p, lt, u, publication.PlanOptions{}); err == nil {
			t.Fatalf("mutator %d: want error", i)
		}
	}
	if _, err := publication.PlanSnapshot(content, 0, tv, pubID, now, "https://example.com/x",
		publication.PlanOptions{}); err == nil {
		t.Fatalf("version 0: want error")
	}
	// Render entry validation.
	if _, _, err := publication.Render(context.Background(), publication.RenderSnapshot{}); err == nil {
		t.Fatalf("Render empty snapshot: want error")
	}
}

func TestCoverGapOutboxWorkerMatrix(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()
	ob := publication.NewOutbox(db)

	if err := ob.InsertUnique(ctx, "", primitive.NewObjectID(), nil); err == nil {
		t.Fatalf("InsertUnique empty type: want error")
	}
	if err := ob.InsertUnique(ctx, "t", primitive.NilObjectID, nil); err == nil {
		t.Fatalf("InsertUnique zero id: want error")
	}
	pubID := primitive.NewObjectID()
	if err := ob.InsertUnique(ctx, "content.publish", pubID, nil); err != nil {
		t.Fatalf("InsertUnique nil payload: %v", err)
	}
	// Duplicate insert is an idempotent no-op.
	if err := ob.InsertUnique(ctx, "content.publish", pubID, map[string]any{"a": 1}); err != nil {
		t.Fatalf("InsertUnique dup: %v", err)
	}

	// Worker defaults + cancelled-before-start Run.
	w := publication.NewOutboxWorker(db, func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
		return nil
	}, publication.WorkerOptions{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(cancelled)
	if n, err := w.Backlog(ctx); err != nil || n != 1 {
		t.Fatalf("Backlog = %d, %v; want 1", n, err)
	}

	// Run with cancel-during-poll: drains the pending row (deliver ok).
	w2 := publication.NewOutboxWorker(db, func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
		return nil
	}, publication.WorkerOptions{PollInterval: 20 * time.Millisecond, WorkerID: "gap-worker"})
	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w2.Run(runCtx); close(done) }()
	select {
	case <-done:
		t.Fatalf("Run returned before cancel")
	case <-time.After(300 * time.Millisecond):
	}
	runCancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not stop after cancel")
	}
	if n, _ := w2.Backlog(ctx); n != 0 {
		t.Fatalf("Backlog after drain = %d, want 0", n)
	}

	// Delivery failure schedules a retry (row stays pending, attempt grows).
	pubID2 := primitive.NewObjectID()
	if err := ob.InsertUnique(ctx, "content.publish", pubID2, map[string]any{}); err != nil {
		t.Fatalf("seed fail row: %v", err)
	}
	w3 := publication.NewOutboxWorker(db, func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
		return errGapDelivery
	}, publication.WorkerOptions{Backoff: func(a int) time.Duration { return time.Hour }})
	didWork, perr := w3.ProcessNext(ctx)
	if perr == nil || !didWork {
		t.Fatalf("ProcessNext deliver-fail: work=%v err=%v, want work=true + propagated error", didWork, perr)
	}
	if n := outboxCount(t, db, "content.publish", pubID2); n != 1 {
		t.Fatalf("failed row lost")
	}
}

var errGapDelivery error = errDummy("delivery boom")

type errDummy string

func (e errDummy) Error() string { return string(e) }

func TestCoverGapScannerRunAndDegraded(t *testing.T) {
	f := newRecFixture(t, nil)
	if f.sc == nil {
		t.Fatalf("fixture scanner nil")
	}
	if f.sc.Degraded() {
		t.Fatalf("Degraded initially")
	}
	f.sc.ResetDegraded()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.sc.Run(ctx); close(done) }()
	// Give the background Run a chance to claim the running latch (the
	// dedicated double-run branch is covered deterministically in-package).
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("Scanner Run did not stop")
	}
	// MigrationState on empty DB.
	if st, err := f.sc.MigrationState(context.Background()); err != nil || st != "" {
		t.Fatalf("MigrationState = %q, %v", st, err)
	}
}

func TestCoverGapRepositoryValidationAndFaults(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()

	if err := repo.InsertStaged(ctx, nil); publication.CodeOf(err) != publication.CodeValidation {
		t.Fatalf("InsertStaged nil: %v", err)
	}
	base := stagedPub(primitive.NewObjectID())
	for i, mut := range []func(*publication.Publication){
		func(p *publication.Publication) { p.ContentID = primitive.NilObjectID },
		func(p *publication.Publication) { p.FullPath = "" },
		func(p *publication.Publication) { p.ContentHash = "" },
		func(p *publication.Publication) { p.LogicalPublishedAt = time.Time{} },
	} {
		p := *base
		mut(&p)
		if err := repo.InsertStaged(ctx, &p); publication.CodeOf(err) != publication.CodeValidation {
			t.Fatalf("InsertStaged case %d: %v", i, err)
		}
	}

	if _, err := repo.GetByID(ctx, primitive.NewObjectID()); publication.CodeOf(err) != publication.CodeNotFound {
		t.Fatalf("GetByID missing: %v", err)
	}
	if _, err := repo.ListHistory(ctx, primitive.NewObjectID()); err != nil {
		t.Fatalf("ListHistory missing: %v", err)
	}
	// Transport errors surface raw.
	bdb := testutil.MustConnectBrokenDB(t)
	brepo := publication.NewRepository(bdb, nil)
	if _, err := brepo.GetByID(ctx, primitive.NewObjectID()); err == nil {
		t.Fatalf("broken GetByID: want error")
	}
	if _, err := brepo.ListHistory(ctx, primitive.NewObjectID()); err == nil {
		t.Fatalf("broken ListHistory: want error")
	}

	// MarkFailed matrix.
	if err := repo.MarkFailed(ctx, primitive.NewObjectID(), "x"); publication.CodeOf(err) != publication.CodeNotFound {
		t.Fatalf("MarkFailed missing: %v", err)
	}
	contentID := seedContent(t, db)
	staged := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, staged); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.MarkFailed(ctx, staged.ID, "render boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := repo.MarkFailed(ctx, staged.ID, "again"); err != nil {
		t.Fatalf("MarkFailed idempotent: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, staged.ID, nil); err == nil {
		t.Fatalf("ActivateCAS on failed: want invalid transition")
	} else if publication.CodeOf(err) != publication.CodeInvalidTransition {
		t.Fatalf("ActivateCAS on failed: %v", err)
	}

	// UnpublishCAS stale-expected conflict.
	staged2 := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, staged2); err != nil {
		t.Fatalf("InsertStaged2: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, staged2.ID, nil); err != nil {
		t.Fatalf("ActivateCAS2: %v", err)
	}
	ghost := primitive.NewObjectID()
	if _, err := repo.UnpublishCAS(ctx, contentID, &ghost, publication.Attribution{}); publication.CodeOf(err) != publication.CodeConflict {
		t.Fatalf("UnpublishCAS stale expected: %v", err)
	}
}

func TestCoverGapServiceValidationAndEffects(t *testing.T) {
	purges := 0
	purgeErr := false
	audits := 0
	s := newSagaSetup(t, nil, nil, publication.Options{
		Purge: func(ctx context.Context, urls []string) error {
			purges++
			if purgeErr {
				return errDummy("cdn down")
			}
			return nil
		},
		Audit: func(ctx context.Context, action string, fields map[string]any) { audits++ },
	})
	ctx := context.Background()

	if _, err := s.svc.Publish(ctx, publication.PublishRequest{}); err == nil {
		t.Fatalf("Publish zero id: want error")
	}
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: primitive.NewObjectID()}); err == nil {
		t.Fatalf("Publish unknown content: want error")
	}
	badTpl := primitive.NewObjectID()
	badID := primitive.NewObjectID()
	_, err := s.db.Collection("content").InsertOne(ctx, map[string]any{
		"_id": badID, "template_id": badTpl, "title": "Bad", "slug": "bad",
		"folder_path": "/bad", "full_path": "/bad//path",
		"data": map[string]any{}, "published": false,
	})
	if err != nil {
		t.Fatalf("seed bad-path content: %v", err)
	}
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: badID, ContentVersion: 1}); err == nil {
		t.Fatalf("Publish bad path: want error")
	}
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{}); err == nil {
		t.Fatalf("Unpublish zero id: want error")
	}
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: primitive.NewObjectID()}); err == nil {
		t.Fatalf("Unpublish unknown: want error")
	}
	// No active → idempotent nil.
	noidTpl, _ := seedSagaTemplateVersion(t, s.db, "", false)
	noid := seedSagaContent(t, s.db, noidTpl, "/news/gap-noid", 1, nil)
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: noid}); err != nil {
		t.Fatalf("Unpublish no active: %v", err)
	}
	if _, err := s.svc.Rollback(ctx, publication.RollbackRequest{}); err == nil {
		t.Fatalf("Rollback zero ids: want error")
	}
	if _, err := s.svc.Rollback(ctx, publication.RollbackRequest{
		ContentID: primitive.NewObjectID(), SourcePublicationID: primitive.NewObjectID()}); err == nil {
		t.Fatalf("Rollback unknown: want error")
	}

	// Full publish exercises purge (error swallowed) + audit sink.
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/gap-effects", 1, nil)
	purgeErr = true
	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if purges == 0 || audits == 0 {
		t.Fatalf("purge/audit sinks not hit: %d %d", purges, audits)
	}
	// Unpublish with wrong expected active → conflict.
	ghost := primitive.NewObjectID()
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: cid, ExpectedActiveID: &ghost}); err == nil {
		t.Fatalf("Unpublish stale expected: want error")
	}
	// Rollback from the retained publication (exact-bytes path).
	rb, err := s.svc.Rollback(ctx, publication.RollbackRequest{ContentID: cid, SourcePublicationID: res.PublicationID})
	if err != nil {
		t.Fatalf("Rollback exact: %v", err)
	}
	if rb.PublicationID == res.PublicationID {
		t.Fatalf("rollback must mint a new publication")
	}
	if rb.PublicationID.IsZero() {
		t.Fatalf("rollback zero id")
	}
}
