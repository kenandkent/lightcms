package staticdelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type commandSource struct {
	active map[primitive.ObjectID]ex.PageSource
	html   map[primitive.ObjectID]string
}

type commandRecordingRemote struct {
	runnerRemote
	files []UploadFile
}

func (r *commandRecordingRemote) UploadAssets(_ context.Context, _ Target, files []UploadFile) (CompletionToken, error) {
	r.files = append([]UploadFile(nil), files...)
	return "ephemeral-jwt", nil
}

func (s *commandSource) Resolve(_ context.Context, i ex.Item) (ex.PageSource, error) {
	p := s.active[i.ContentID]
	if p.PublicationID.IsZero() {
		return p, failure(CodeSourceNotExportable, "no published version", nil)
	}
	return p, nil
}
func (s *commandSource) Open(_ context.Context, p ex.PageSource) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.html[p.PublicationID])), nil
}
func TestCommandsFrozenExportPreviewAndDurableDeployment(t *testing.T) {
	repo, ctx := connectDeliveryTestDB(t)
	root := t.TempDir()
	cfg := Config{ProjectRoot: root, Root: filepath.Join(root, "private")}
	src := &commandSource{active: map[primitive.ObjectID]ex.PageSource{}, html: map[primitive.ObjectID]string{}}
	cid, pid := primitive.NewObjectID(), primitive.NewObjectID()
	body := `<!doctype html><html><head><title>Original</title></head><body><h1>Original</h1></body></html>`
	src.active[cid] = ex.PageSource{ContentID: cid, PublicationID: pid, FullPath: "/report/a", PublicURL: "https://cms.example/report/a", SourceSHA256: releaseHash([]byte(body))}
	src.html[pid] = body
	exports := ex.NewService(src, nil, ex.Options{ProjectRoot: root, Root: cfg.Root})
	accounts := NewAccountService(repo, nil, func(context.Context, string, string, string) (ConnectionCheck, error) {
		return ConnectionCheck{Readable: true}, nil
	})
	account, err := accounts.Create(ctx, "测试账号", "abcdef0123456789abcdef0123456789", "private-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = accounts.Verify(ctx, account.ID, ""); err != nil {
		t.Fatal(err)
	}
	target := releaseTarget()
	target.Status = "ready"
	target.AccountRef = account.ID
	target.RemoteVersionID = "old"
	if _, err = repo.collection("targets").InsertOne(ctx, target); err != nil {
		t.Fatal(err)
	}
	actor := ActorSnapshot{UserID: "user", Owner: "user", Role: "admin"}
	svc := NewCommandService(repo, CommandOptions{Source: src, Exports: exports, Store: NewFileReleaseStore(cfg), Accounts: accounts, Authorize: func(context.Context, ActorSnapshot, bool, bool) error { return nil }})
	first, err := svc.SubmitExport(ctx, actor, "export-key", []ex.Item{{ContentID: cid}})
	if err != nil {
		t.Fatal(err)
	}
	newer := src.active[cid]
	newer.PublicationID = primitive.NewObjectID()
	src.active[cid] = newer
	replay, err := svc.SubmitExport(ctx, actor, "export-key", []ex.Item{{ContentID: cid}})
	if err != nil || replay.ID != first.ID {
		t.Fatal("replay changed export job", err)
	}
	remote := &commandRecordingRemote{runnerRemote: runnerRemote{targetRemote: targetRemote{state: RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}}}
	runner := svc.NewRunner(RunnerDependencies{Owner: "test", Remote: func(context.Context, Job, Target) (Remote, error) { return remote, nil }, Verify: commandPublicVerifier(t)})
	if err = runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := svc.GetExport(ctx, actor, first.ExportID)
	if err != nil || record.State != "ready" || record.Frozen.Pages[0].PublicationID != pid {
		t.Fatal("export not frozen/ready", err)
	}
	req := DeployRequest{ExportIDs: []primitive.ObjectID{first.ExportID}, ExpectedRevision: 0, ExpectedConfigRevision: target.ConfigRevision, ManualScriptReview: true}
	preparer := jobs.NewRunner(jobs.NewRepository(repo.db, 10), 1)
	svc.EnablePreviewJobs(preparer)
	if _, err := svc.SubmitPreview(ctx, actor, "", target.ID, req); CodeOf(err) != CodeIdempotencyRequired {
		t.Fatalf("missing preview key: %v", err)
	}
	if _, err := svc.GetPreviewJob(ctx, actor, primitive.NewObjectID()); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing preview returned wrong code: %v", err)
	}
	prepared, err := svc.SubmitPreview(ctx, actor, "async-preview", target.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != "queued" || prepared.Preview != nil {
		t.Fatal("preview built during HTTP submission")
	}
	if err := preparer.RunOnce(ctx, "delivery_preview"); err != nil {
		t.Fatal(err)
	}
	prepared, err = svc.GetPreviewJob(ctx, actor, prepared.JobID)
	if err != nil || prepared.State != "succeeded" || prepared.Preview == nil || len(prepared.Preview.Changes) != 1 {
		t.Fatalf("async preview unavailable: %+v %v", prepared, err)
	}
	preview, err := svc.PreviewDeployment(ctx, actor, target.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 1 || preview.Changes[0].Path != "/report/a" || remote.activateCalls != 0 {
		t.Fatal("preview mutated remote or wrong changes")
	}
	req.ConfirmationDigest = preview.Digest
	changed := req
	changed.ManualScriptReview = false
	if _, err = svc.SubmitDeployment(ctx, actor, "changed-confirmation", target.ID, changed); CodeOf(err) != CodeConfirmationRequired {
		t.Fatal("altered preview request accepted", err)
	}
	if _, err = repo.collection("previews").UpdateOne(ctx, bson.M{"_id": preview.Digest}, bson.M{"$set": bson.M{"expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SubmitDeployment(ctx, actor, "expired-confirmation", target.ID, req); CodeOf(err) != CodeConfirmationRequired {
		t.Fatal("expired confirmation accepted", err)
	}
	if _, err = repo.collection("previews").UpdateOne(ctx, bson.M{"_id": preview.Digest}, bson.M{"$set": bson.M{"expires_at": time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	job, err := svc.SubmitDeployment(ctx, actor, "deploy-key", target.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	remote.releaseID = job.ReleaseID.Hex()
	duplicate, err := svc.SubmitDeployment(ctx, actor, "deploy-key", target.ID, req)
	if err != nil || duplicate.ID != job.ID {
		t.Fatal("duplicate job", err)
	}
	if err = runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := svc.GetJob(ctx, actor, job.ID)
	if err != nil || done.State != JobSucceeded || remote.activateCalls != 1 {
		t.Fatalf("not deployed: %+v err=%v calls=%d", done, err, remote.activateCalls)
	}
	if len(remote.files) != 2 || remote.files[0].PublicPath != "/report/a" || remote.files[0].MIME != "text/html; charset=utf-8" || remote.files[1].PublicPath != "/.__lc_delivery/health" || remote.files[1].MIME != "application/json" {
		t.Fatalf("runner did not upload the direct HTML route: %+v", remote.files)
	}
	duplicate, err = svc.SubmitDeployment(ctx, actor, "deploy-key", target.ID, req)
	if err != nil || duplicate.ID != job.ID {
		t.Fatal("successful replay rechecked stale target", err)
	}
	latest, _ := repo.GetTarget(ctx, target.ID)
	manifest, err := svc.store.Load(ctx, latest.CurrentReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Pages["/report/a"].PublicationID != pid {
		t.Fatal("deployed mutable latest")
	}
	var pageStatus bson.M
	if err := repo.collection("page_status").FindOne(ctx, bson.M{"content_id": cid, "release_id": job.ReleaseID}).Decode(&pageStatus); err != nil {
		t.Fatal("verified page status missing", err)
	}
	if pageStatus["url"] != target.Origin+"/report/a" || pageStatus["publication_id"] != pid || pageStatus["html"] != nil {
		t.Fatalf("page status is not a frozen metadata projection: %+v", pageStatus)
	}
	var releaseRecord struct {
		State string `bson:"state"`
	}
	if err = repo.collection("releases").FindOne(ctx, bson.M{"_id": job.ReleaseID}).Decode(&releaseRecord); err != nil || releaseRecord.State != "committed" {
		t.Fatal("release bookkeeping not committed", err)
	}
	var op bson.M
	if err = repo.collection("operations").FindOne(ctx, bson.M{"_id": job.OperationID}).Decode(&op); err != nil {
		t.Fatal(err)
	}
	// Deploy B, then update A: every version must retain unrelated pages.
	bID := primitive.NewObjectID()
	for n, contentID := range []primitive.ObjectID{bID, cid} {
		path := "/report/b"
		if n == 1 {
			path = "/report/a"
		}
		publicationID := primitive.NewObjectID()
		html := fmt.Sprintf("<!doctype html><html><head><title>Report %d</title></head><body><p>Version %d</p></body></html>", n, n)
		src.active[contentID] = ex.PageSource{ContentID: contentID, PublicationID: publicationID, FullPath: path, PublicURL: "https://cms.example" + path, SourceSHA256: releaseHash([]byte(html))}
		src.html[publicationID] = html
		exportJob, e := svc.SubmitExport(ctx, actor, fmt.Sprintf("export-%d", n), []ex.Item{{ContentID: contentID}})
		if e != nil {
			t.Fatal(e)
		}
		if e = runner.RunOnce(ctx); e != nil {
			t.Fatal(e)
		}
		targetNow, _ := repo.GetTarget(ctx, target.ID)
		request := DeployRequest{ExportIDs: []primitive.ObjectID{exportJob.ExportID}, ExpectedRevision: targetNow.CurrentRevision, ExpectedConfigRevision: targetNow.ConfigRevision, ManualScriptReview: true}
		p, e := svc.PreviewDeployment(ctx, actor, target.ID, request)
		if e != nil || p.PreservedPages != 1 {
			t.Fatalf("preservation preview: %+v %v", p, e)
		}
		request.ConfirmationDigest = "tampered"
		if _, e = svc.SubmitDeployment(ctx, actor, fmt.Sprintf("bad-%d", n), target.ID, request); CodeOf(e) != CodeConfirmationRequired {
			t.Fatalf("tampered confirmation accepted: %v", e)
		}
		request.ConfirmationDigest = p.Digest
		deployment, e := svc.SubmitDeployment(ctx, actor, fmt.Sprintf("deploy-%d", n), target.ID, request)
		if e != nil {
			t.Fatal(e)
		}
		remote.releaseID = deployment.ReleaseID.Hex()
		if e = runner.RunOnce(ctx); e != nil {
			t.Fatal(e)
		}
		finished, e := svc.GetJob(ctx, actor, deployment.ID)
		if e != nil || finished.State != JobSucceeded {
			t.Fatalf("followup failed: %+v %v", finished, e)
		}
		latest, _ = repo.GetTarget(ctx, target.ID)
		manifest, e = svc.store.Load(ctx, latest.CurrentReleaseID)
		if e != nil || len(manifest.Pages) != 2 || manifest.Pages[path].PublicationID != publicationID {
			t.Fatalf("lost page or wrong update: %v", e)
		}
	}
	history, e := svc.History(ctx, actor, target.ID, 0, 20)
	if e != nil || len(history) != 3 {
		t.Fatalf("history %d: %v", len(history), e)
	}
	// Every maintenance action uses the same frozen preview/confirmation and
	// remote verification pipeline; configuration must not change beforehand.
	svc.options.CheckCandidateOrigin = func(context.Context, Target) error { return nil }
	svc.options.Defaults = func(context.Context, string) (ex.Resource, ex.Resource, error) {
		share := []byte("share-fixture")
		icon := []byte("icon-fixture")
		return ex.Resource{URL: "https://cms.example/share.png", MIME: "image/png", Data: share, SHA256: releaseHash(share)}, ex.Resource{URL: "https://cms.example/icon.png", MIME: "image/png", Data: icon, SHA256: releaseHash(icon)}, nil
	}
	for n, maintenance := range []DeployRequest{
		{Operation: "rollback_pages", Paths: []string{"/report/a"}, SourceReleaseID: job.ReleaseID.Hex()},
		{Operation: "withdraw", Paths: []string{"/report/b"}},
		{Operation: "rollback_site", SourceReleaseID: job.ReleaseID.Hex()},
		{Operation: "change_environment", NewEnvironment: "production"},
		{Operation: "change_origin", NewOrigin: "https://new.example.com"},
	} {
		before, err := repo.GetTarget(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		maintenance.ExpectedRevision = before.CurrentRevision
		maintenance.ExpectedConfigRevision = before.ConfigRevision
		maintenance.ManualScriptReview = true
		preview, err := svc.PreviewDeployment(ctx, actor, target.ID, maintenance)
		if err != nil {
			t.Fatalf("%s preview: %v", maintenance.Operation, err)
		}
		maintenance.ConfirmationDigest = preview.Digest
		deployment, err := svc.SubmitDeployment(ctx, actor, fmt.Sprintf("maintenance-%d", n), target.ID, maintenance)
		if err != nil {
			t.Fatal(err)
		}
		remote.releaseID = deployment.ReleaseID.Hex()
		remote.onActivate = func() {
			current, err := repo.GetTarget(ctx, target.ID)
			if err != nil || current.Origin != before.Origin || current.Environment != before.Environment || current.ConfigRevision != before.ConfigRevision {
				t.Error("configuration committed before remote verification")
			}
		}
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		finished, err := svc.GetJob(ctx, actor, deployment.ID)
		if err != nil || finished.State != JobSucceeded {
			t.Fatalf("%s did not finish: %+v %v", maintenance.Operation, finished, err)
		}
		now, err := repo.GetTarget(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := svc.store.Load(ctx, now.CurrentReleaseID)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Pages["/report/a"].PublicationID != pid {
			t.Fatalf("%s did not preserve historical publication", maintenance.Operation)
		}
		if n >= 1 && len(manifest.Pages) != 1 {
			t.Fatalf("%s retained withdrawn page", maintenance.Operation)
		}
		if maintenance.Operation == "change_environment" && (now.Environment != "production" || now.ConfigRevision != before.ConfigRevision+1) {
			t.Fatal("environment not committed atomically")
		}
		if maintenance.Operation == "change_origin" && (now.Origin != maintenance.NewOrigin || now.ConfigRevision != before.ConfigRevision+1) {
			t.Fatal("origin not committed atomically")
		}
	}
	remote.onActivate = nil
	// A queued export can be cancelled idempotently and cannot execute afterward.
	cancelled, e := svc.SubmitExport(ctx, actor, "cancel-export", []ex.Item{{ContentID: cid}})
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 2; n++ {
		if _, e = svc.Cancel(ctx, actor, cancelled.ID, "cancel-key"); e != nil {
			t.Fatal(e)
		}
	}
	if e = runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	cancelled, e = svc.GetJob(ctx, actor, cancelled.ID)
	if e != nil || cancelled.State != JobCancelled {
		t.Fatal("cancelled export executed", e)
	}
	denied := NewCommandService(repo, CommandOptions{Authorize: func(context.Context, ActorSnapshot, bool, bool) error {
		return failure(CodePermissionDenied, "denied", nil)
	}})
	before, _ := repo.collection("operations").CountDocuments(ctx, bson.M{})
	if _, e = denied.SubmitExport(ctx, actor, "denied", []ex.Item{{ContentID: cid}}); CodeOf(e) != CodePermissionDenied {
		t.Fatal(e)
	}
	after, _ := repo.collection("operations").CountDocuments(ctx, bson.M{})
	if before != after {
		t.Fatal("denied request mutated operations")
	}
}

// Real HTTP/TLS verification of the frozen bytes, MIME, HEAD, cache and identity
// contract; only the Cloudflare management API is simulated by runnerRemote.
func commandPublicVerifier(t *testing.T) func(context.Context, ReleaseManifest) error {
	t.Helper()
	var release ReleaseManifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.__lc_delivery/health" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "installation_id": release.InstallationID, "target_id": release.TargetID.Hex(), "release_id": release.ID.Hex()})
			return
		}
		path := r.URL.Path
		if p, ok := release.Pages[path]; ok {
			path = p.File
		}
		f, ok := release.Files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-LC-Release", release.ID.Hex())
		w.Header().Set("Content-Type", f.MIME)
		w.Header().Set("ETag", `"`+f.SHA256+`"`)
		cache := "public,max-age=31536000,immutable"
		if strings.HasPrefix(f.MIME, "text/html") {
			cache = "no-cache"
		}
		w.Header().Set("Cache-Control", cache)
		if r.Method != "HEAD" {
			_, _ = w.Write(f.Data)
		}
	}))
	t.Cleanup(server.Close)
	v := NewReleaseVerifier()
	v.client = &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Host = strings.TrimPrefix(server.URL, "https://")
		clone.URL = &u
		return server.Client().Transport.RoundTrip(clone)
	})}
	v.wait = func(context.Context, time.Duration) error { return nil }
	return func(ctx context.Context, m ReleaseManifest) error { release = m; return v.Check(ctx, m) }
}
