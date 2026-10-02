package generation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func mustBody(r generation.GenerateRequest, v int64) []byte {
	b, _ := json.Marshal(map[string]any{"template": r.Template, "title": r.Title, "slug": r.Slug, "folder_path": r.FolderPath, "mode": r.Mode, "upsert": r.Upsert, "data": r.Data, "expected_template_version": v})
	return b
}

type reviewTakeoverStore struct {
	storage.Store
	phase    string
	takeOver func(primitive.ObjectID) error
}

func (s reviewTakeoverStore) Verify(ctx context.Context, obj storage.StagedObject) error {
	if s.phase == "verify" {
		if err := s.takeOver(obj.PublicationID); err != nil {
			return err
		}
		return fmt.Errorf("verification interrupted by takeover")
	}
	return s.Store.Verify(ctx, obj)
}
func (s reviewTakeoverStore) Activate(ctx context.Context, obj storage.StagedObject, path string) error {
	if s.phase == "activate" {
		if err := s.takeOver(obj.PublicationID); err != nil {
			return err
		}
		return fmt.Errorf("activation interrupted by takeover")
	}
	return s.Store.Activate(ctx, obj, path)
}

func TestFinalReviewLostWorkerMustNotFailNewOwnersPublication(t *testing.T) {
	for _, phase := range []string{"verify", "activate"} {
		t.Run(phase, func(t *testing.T) {
			s := newGenSetup(t, generation.Options{})
			_, _, v := s.seedTemplate(t, "lost-owner", "", nil)
			a := adminActor()
			store := reviewTakeoverStore{Store: s.store, phase: phase, takeOver: func(pid primitive.ObjectID) error {
				var op idempotency.Operation
				if err := s.db.FindOne(context.Background(), idempotency.CollectionName, bson.M{"publication_id": pid}, &op); err != nil {
					return err
				}
				if _, err := s.db.Collection(idempotency.CollectionName).UpdateOne(context.Background(), bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
					return err
				}
				_, err := s.idem.TakeOver(context.Background(), op.ID, op.LeaseGeneration)
				return err
			}}
			saga := publication.NewService(s.db, s.repo, store, publication.Options{Templates: s.tpls, Idem: s.idem})
			gen := generation.NewService(s.db, generation.Options{Templates: s.tpls, Pubs: saga, PubRepo: s.repo, Idem: s.idem})
			r := generation.GenerateRequest{Template: "lost-owner", Title: "Owner", Slug: "owner", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Owner"}}
			if _, err := gen.Generate(pubCtx(a, "owner", r, v), a, r); err == nil {
				t.Fatal("stale worker succeeded")
			}
			if n := countDocs(t, s.db, "content_publications", bson.M{"status": "failed"}); n != 0 {
				t.Fatal("old worker poisoned new owner's publication")
			}
			if n := countDocs(t, s.db, publication.CollectionOutbox, bson.M{}); n != 0 {
				t.Fatal("old worker created an outbox event")
			}
		})
	}
}
func mustID(t *testing.T, s string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFinalReviewTextRemainsLiteral(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "literal", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "literal", Title: "Literal", Slug: "literal", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "<b>Literal & text</b>"}}
	if _, err := s.gen.Generate(pubCtx(a, "literal", r, v), a, r); err != nil {
		t.Fatal(err)
	}
	html := readCanonical(t, s.root, "/literal")
	if !strings.Contains(html, "&lt;b&gt;Literal &amp; text&lt;/b&gt;") {
		t.Fatalf("text field became markup: %s", html)
	}
}

func TestFinalReviewCreateOnlyReplay(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "create-only", "", nil)
	a := authed(generation.ScopeContentCreate, generation.ScopeContentPublish)
	r := generation.GenerateRequest{Template: "create-only", Title: "Create", Slug: "create", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Create"}}
	first, err := s.gen.Generate(pubCtx(a, "create", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.gen.Generate(pubCtx(a, "create", r, v), a, r)
	if err != nil {
		t.Fatalf("original creator cannot replay: %v", err)
	}
	if *first.PublicationID != *replay.PublicationID {
		t.Fatal("replay created another publication")
	}
	if _, err := s.gen.Generate(pubCtx(a, "new-command", r, v), a, r); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("creator edited an existing page with a new key: %v", err)
	}
}

func TestFinalReviewCreateCrashWithoutUpsert(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "crash-create", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "crash-create", Title: "Crash", Slug: "crash", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Crash"}}
	// Persist a content write and bind as the original worker would do,
	// then expire its lease before any Publication was staged.
	draft := r
	draft.Mode = "draft"
	d, err := s.gen.Generate(context.Background(), a, draft)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.idem.Begin(pubCtx(a, "crash", r, v), a.Owner(), "POST", "/api/v1/page-generation", "crash", mustBody(r, v))
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t, d.ID)
	if err := s.idem.BindContentAndVersion(context.Background(), nil, op.ID, id, 1, "/crash"); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Collection("idempotency_records").UpdateOne(context.Background(), bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.gen.Generate(pubCtx(a, "crash", r, v), a, r); err != nil {
		t.Fatalf("bound create retry failed: %v", err)
	}
}

func TestFinalReviewReplayAfterDeprecation(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	id, _, v := s.seedTemplate(t, "deprecated-replay", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "deprecated-replay", Title: "Replay", Slug: "replay", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Replay"}}
	first, err := s.gen.Generate(pubCtx(a, "deprecated", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.tpls.Update(context.Background(), id, v, templatecontract.TemplateInput{Slug: r.Template, Name: r.Template + " name", Category: "news", Status: "deprecated", HTMLLayout: genLayout, Fields: []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text", Required: true}, {Name: "body", Label: "Body", Type: "textarea"}}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.gen.Generate(pubCtx(a, "deprecated", r, v), a, r)
	if err != nil {
		t.Fatalf("completed replay blocked by current template: %v", err)
	}
	if *first.PublicationID != *replay.PublicationID {
		t.Fatal("different publication")
	}
}

func TestFinalReviewReplayWarnings(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "warnings", "", []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text", Required: true}, {Name: "body", Label: "Body", Type: "textarea", Default: "Default"}})
	a := adminActor()
	r := generation.GenerateRequest{Template: "warnings", Title: "Warnings", Slug: "warnings", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Warnings"}}
	first, err := s.gen.Generate(pubCtx(a, "warnings", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.gen.Generate(pubCtx(a, "warnings", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Warnings) == 0 || len(first.Warnings) != len(replay.Warnings) {
		t.Fatalf("warnings original=%v replay=%v", first.Warnings, replay.Warnings)
	}
	// A post-activation/cache crash can be recovered after the page was
	// subsequently unpublished; both IDs and warnings remain the original result.
	if err := s.saga.Unpublish(context.Background(), publication.UnpublishRequest{ContentID: mustID(t, first.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Collection(idempotency.CollectionName).UpdateOne(context.Background(), bson.M{"key": "warnings"}, bson.M{"$set": bson.M{"state": "processing", "attempt_state": "processing", "response": nil, "status_code": nil, "processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.gen.Generate(pubCtx(a, "warnings", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Warnings) != len(first.Warnings) || *recovered.PublicationID != *first.PublicationID {
		t.Fatalf("committed response drifted: %+v", recovered)
	}
}

type finalToggleLimiter struct{ denied bool }

func (l *finalToggleLimiter) Allow(context.Context, generation.Actor) (bool, int) {
	return !l.denied, 5
}

func TestFinalReviewReplayIgnoresMutableAvailability(t *testing.T) {
	l := &finalToggleLimiter{}
	down := false
	s := newGenSetup(t, generation.Options{Limiter: l, StoreAvailable: func(context.Context) error {
		if down {
			return fmt.Errorf("storage unavailable")
		}
		return nil
	}})
	_, _, v := s.seedTemplate(t, "availability", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "availability", Title: "Availability", Slug: "availability", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Availability"}}
	first, err := s.gen.Generate(pubCtx(a, "available", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	l.denied = true
	down = true
	replay, err := s.gen.Generate(pubCtx(a, "available", r, v), a, r)
	if err != nil || *replay.PublicationID != *first.PublicationID {
		t.Fatalf("mutable gates blocked committed replay: %+v %v", replay, err)
	}
	if _, err = s.gen.Generate(pubCtx(a, "new-unavailable", r, v), a, r); generation.CodeOf(err) != generation.CodeRateLimited {
		t.Fatalf("fresh command bypassed limiter: %v", err)
	}
	l.denied = false
	if _, err = s.gen.Generate(pubCtx(a, "new-down", r, v), a, r); generation.CodeOf(err) != generation.CodeStoreUnavailable {
		t.Fatalf("fresh command bypassed storage gate: %v", err)
	}
}

func TestFinalReviewMigrateSlugCurrentVersion(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	id, oldID, _ := s.seedTemplate(t, "old-slug", "", nil)
	if _, err := s.gen.MigrateSlug(context.Background(), adminActor(), id, "new-slug"); err != nil {
		t.Fatal(err)
	}
	v, err := s.tpls.GetCurrent(context.Background(), "new-slug")
	if err != nil {
		t.Fatal(err)
	}
	if v.Slug != "new-slug" || v.Version != 2 {
		t.Fatalf("current immutable contract not migrated: %+v", v)
	}
	old, err := s.tpls.GetVersion(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Slug != "old-slug" || old.Version != 1 {
		t.Fatal("historical contract changed")
	}
}

func TestFinalReviewHistoryRecoveryAndPreconditionHash(t *testing.T) {
	for _, mode := range []string{"rollback", "revert", "restore"} {
		t.Run(mode, func(t *testing.T) {
			s := newGenSetup(t, generation.Options{})
			_, _, v := s.seedTemplate(t, "history", "", nil)
			a := adminActor()
			r := generation.GenerateRequest{Template: "history", Title: "History", Slug: "history", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "History"}}
			first, err := s.gen.Generate(pubCtx(a, "first", r, v), a, r)
			if err != nil {
				t.Fatal(err)
			}
			cid := mustID(t, first.ID)
			pid := mustID(t, *first.PublicationID)
			kctx := generation.WithIdempotency(context.Background(), generation.IdempotencyParams{Owner: a.Owner(), Method: "POST", Path: "/history/" + mode, Key: mode})
			call := func(expected *primitive.ObjectID) (string, error) {
				switch mode {
				case "rollback":
					o, e := s.gen.RollbackPublication(kctx, a, cid, pid, expected)
					return o.PublicationID, e
				case "revert":
					o, e := s.gen.RevertLive(kctx, a, cid, pid, expected)
					return o.PublicationID, e
				default:
					o, e := s.gen.RestoreAndPublish(kctx, a, cid, 1, expected)
					return o.PublicationID, e
				}
			}
			p, err := call(&pid)
			if err != nil {
				t.Fatal(err)
			}
			// Successful side effect, crash before response caching; takeover must
			// not reapply expected-active or rewrite the restored ContentVersion.
			_, err = s.db.Collection("idempotency_records").UpdateOne(context.Background(), bson.M{"key": mode}, bson.M{"$set": bson.M{"state": "processing", "attempt_state": "processing", "response": nil, "status_code": nil, "processing_expires_at": time.Now().Add(-time.Minute)}})
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := call(&pid)
			if err != nil {
				t.Fatalf("committed history retry failed: %v", err)
			}
			if recovered != p {
				t.Fatal("retry minted another publication")
			}
			other := primitive.NewObjectID()
			if _, err = call(&other); generation.CodeOf(err) != generation.CodeIdempotencyConflict {
				t.Fatalf("changed expected_active_id must conflict, got %v", err)
			}
		})
	}
}

func TestFinalReviewUpgradeIncludesEveryPage(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	id, _, _ := s.seedTemplate(t, "many-pages", "", nil)
	rows := make([]any, 501)
	for i := range rows {
		rows[i] = bson.M{"_id": primitive.NewObjectID(), "template_id": id, "path_scope": "live", "path_active": true, "full_path": fmt.Sprintf("/many/%d", i), "canonical_full_path": fmt.Sprintf("/many/%d", i), "current_version": int64(1), "data": bson.M{"headline": "Page"}}
	}
	if _, err := s.db.Collection("content").InsertMany(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	preview, err := s.gen.PreviewUpgrade(context.Background(), adminActor(), "many-pages")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 501 {
		t.Fatalf("upgrade silently omitted pages: %d/501", len(preview.Items))
	}
	job, err := s.gen.StartUpgradeJob(context.Background(), adminActor(), "many-pages")
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Items) != 501 {
		t.Fatalf("job silently omitted pages: %d/501", len(job.Items))
	}
}

func TestFinalReviewOldWorkerCannotCommitAfterRenderTakeover(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "fenced", "", nil)
	a := adminActor()
	saga := publication.NewService(s.db, s.repo, s.store, publication.Options{Templates: s.tpls, Idem: s.idem, Renderer: func(ctx context.Context, in publication.RenderInput) ([]byte, error) {
		var op idempotency.Operation
		if err := s.db.FindOne(ctx, "idempotency_records", bson.M{"publication_id": in.PublicationID}, &op); err != nil {
			return nil, err
		}
		if _, err := s.db.Collection("idempotency_records").UpdateOne(ctx, bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
			return nil, err
		}
		if _, err := s.idem.TakeOver(context.Background(), op.ID, op.LeaseGeneration); err != nil {
			return nil, err
		}
		return publication.DefaultRenderer(ctx, in)
	}})
	gen := generation.NewService(s.db, generation.Options{Templates: s.tpls, Pubs: saga, PubRepo: s.repo, Idem: s.idem})
	r := generation.GenerateRequest{Template: "fenced", Title: "Fenced", Slug: "fenced", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Old worker"}}
	if _, err := gen.Generate(pubCtx(a, "fenced", r, v), a, r); err == nil {
		t.Fatal("stale worker returned success")
	}
	if n := countDocs(t, s.db, "content_publications", bson.M{"status": "active"}); n != 0 {
		t.Fatalf("stale worker committed %d active publications", n)
	}
}

func TestFinalReviewCredentialsDoNotShareOperation(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "credentials", "", nil)
	a, b := adminActor(), adminActor()
	if err := json.Unmarshal([]byte(`{"CredentialOwner":"apikey:first"}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"CredentialOwner":"apikey:second"}`), &b); err != nil {
		t.Fatal(err)
	}
	r := generation.GenerateRequest{Template: "credentials", Title: "Keys", Slug: "keys", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Keys"}}
	if _, err := s.gen.Generate(pubCtx(a, "same-key", r, v), a, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.gen.Generate(pubCtx(b, "same-key", r, v), b, r); generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("second credential inherited first operation: %v", err)
	}
}

func TestFinalReviewRestoreCutoverCrashDoesNotBumpAgain(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v := s.seedTemplate(t, "restore-crash", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "restore-crash", Title: "Restore", Slug: "restore-crash", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Restore"}}
	first, err := s.gen.Generate(pubCtx(a, "initial", r, v), a, r)
	if err != nil {
		t.Fatal(err)
	}
	cid, pid := mustID(t, first.ID), mustID(t, *first.PublicationID)
	crashed := false
	saga := publication.NewService(s.db, s.repo, s.store, publication.Options{Templates: s.tpls, Idem: s.idem, Faults: publication.Faults{BeforeCommit: func(context.Context) error {
		if !crashed {
			crashed = true
			return publication.ErrStopAfterRename
		}
		return nil
	}}})
	gen := generation.NewService(s.db, generation.Options{Templates: s.tpls, Pubs: saga, PubRepo: s.repo, Idem: s.idem})
	ctx := generation.WithIdempotency(context.Background(), generation.IdempotencyParams{Owner: a.Owner(), Method: "POST", Path: "/restore-crash", Key: "restore-crash"})
	if _, err := gen.RestoreAndPublish(ctx, a, cid, 1, &pid); err == nil {
		t.Fatal("crash fixture did not run")
	}
	var op idempotency.Operation
	if err = s.db.FindOne(context.Background(), "idempotency_records", bson.M{"key": "restore-crash"}, &op); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Collection("idempotency_records").UpdateOne(context.Background(), bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := gen.RestoreAndPublish(ctx, a, cid, 1, &pid)
	if err != nil {
		t.Fatalf("cutover recovery: %v", err)
	}
	if res.ContentVersion != 2 || op.PublicationID == nil || res.PublicationID != op.PublicationID.Hex() {
		t.Fatalf("changed durable binding: op=%+v result=%+v", op, res)
	}
	if n := countDocs(t, s.db, "content_versions", bson.M{"content_id": cid}); n != 2 {
		t.Fatalf("restore duplicated ContentVersion: %d", n)
	}
}

func TestFinalReviewUpgradeUncertainCutoverRetainsAttempt(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	id, _, v := s.seedTemplate(t, "job-crash", "", nil)
	a := adminActor()
	r := generation.GenerateRequest{Template: "job-crash", Title: "Upgrade", Slug: "job-crash", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Upgrade"}}
	if _, err := s.gen.Generate(pubCtx(a, "before-job", r, v), a, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.tpls.Update(context.Background(), id, v, templatecontract.TemplateInput{Slug: r.Template, Name: r.Template + " name", Category: "news", Status: "active", HTMLLayout: genLayout + "<!--v2-->", Fields: []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text", Required: true}, {Name: "body", Label: "Body", Type: "textarea"}}}); err != nil {
		t.Fatal(err)
	}
	saga := publication.NewService(s.db, s.repo, s.store, publication.Options{Templates: s.tpls, Idem: s.idem, Faults: publication.Faults{BeforeCommit: func(context.Context) error { return publication.ErrStopAfterRename }}})
	gen := generation.NewService(s.db, generation.Options{Templates: s.tpls, Pubs: saga, PubRepo: s.repo, Idem: s.idem})
	job, err := gen.StartUpgradeJob(context.Background(), a, "job-crash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.RunUpgradeJob(context.Background(), a, job.ID); err != nil {
		t.Fatal(err)
	}
	var op idempotency.Operation
	if err = s.db.FindOne(context.Background(), idempotency.CollectionName, bson.M{"path": "/internal/upgrade-jobs/" + job.ID.Hex() + "/publish"}, &op); err != nil {
		t.Fatal(err)
	}
	if op.AttemptState != idempotency.AttemptProcessing {
		t.Fatalf("uncertain job cutover marked terminal: %s", op.AttemptState)
	}
	if _, err = s.db.Collection(idempotency.CollectionName).UpdateOne(context.Background(), bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.gen.RunUpgradeJob(context.Background(), a, job.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.gen.GetUpgradeJob(context.Background(), a, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Items) != 1 || recovered.Items[0].PublicationID == nil || op.PublicationID == nil || *recovered.Items[0].PublicationID != *op.PublicationID {
		t.Fatalf("job recovery changed Publication: %+v", recovered)
	}
}
