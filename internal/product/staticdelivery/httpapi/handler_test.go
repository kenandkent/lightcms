package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/database"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeploymentRejectsPermissionsAndAccountOverride(t *testing.T) {
	for _, tc := range []struct {
		role, body string
		status     int
	}{
		{"viewer", `{}`, 403},
		{"editor", `{"account_id":"other-account"}`, 400},
		{"editor", `{} {}`, 400},
		{"editor", `{}`, 428},
		{"editor", `{"expected_revision":null,"expected_config_revision":1}`, 428},
	} {
		h := &Handlers{ActorExtractor: func(*http.Request) (*auth.SessionUser, bool) {
			return &auth.SessionUser{ID: "test", Role: tc.role}, true
		}}
		router := mux.NewRouter()
		Register(router, h)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/static-targets/000000000000000000000001/deployments", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("role=%s body=%s status=%d", tc.role, tc.body, w.Code)
		}
	}
}

type httpSource struct {
	page ex.PageSource
	body string
}

func (s httpSource) Resolve(context.Context, ex.Item) (ex.PageSource, error) { return s.page, nil }
func (s httpSource) Open(context.Context, ex.PageSource) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.body)), nil
}

type httpRemote struct {
	state       sd.RemoteState
	release     string
	activations int
}

func (f *httpRemote) Inspect(context.Context, sd.Target) (sd.RemoteState, error) { return f.state, nil }
func (f *httpRemote) Initialize(context.Context, sd.Target, []sd.Module) (sd.RemoteVersion, error) {
	return sd.RemoteVersion{}, fmt.Errorf("not allowed in publication")
}
func (f *httpRemote) UploadAssets(context.Context, sd.Target, []sd.UploadFile) (sd.CompletionToken, error) {
	return "private-test-completion", nil
}
func (f *httpRemote) UploadVersion(context.Context, sd.Target, sd.CompletionToken, []sd.Module) (sd.RemoteVersion, error) {
	return sd.RemoteVersion{ID: "candidate"}, nil
}
func (f *httpRemote) Activate(_ context.Context, t sd.Target, v string) (sd.RemoteDeployment, error) {
	f.activations++
	f.state = sd.RemoteState{Exists: true, SingleVersion: true, InstallationID: t.InstallationID, TargetID: t.ID.Hex(), ReleaseID: f.release, VersionID: v}
	return sd.RemoteDeployment{ID: "deployment", VersionID: v}, nil
}

func TestHTTPPublishedContentToDeploymentAndDownload(t *testing.T) {
	uri := os.Getenv("STATIC_DELIVERY_TEST_MONGODB_URI")
	if uri == "" {
		if os.Getenv("STATIC_DELIVERY_REQUIRE_MONGO") == "1" {
			t.Fatal("isolated Mongo required")
		}
		t.Skip("isolated Mongo unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	db, e := database.Connect(ctx, uri, "static-delivery-test-"+primitive.NewObjectID().Hex())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = db.Collection("_cleanup").Database().Drop(context.Background())
		_ = db.Disconnect(context.Background())
	})
	repo := sd.NewRepository(db)
	if e = repo.EnsureIndexes(ctx); e != nil {
		t.Fatal(e)
	}
	accounts := sd.NewAccountService(repo, nil, func(context.Context, string, string, string) (sd.ConnectionCheck, error) {
		return sd.ConnectionCheck{Readable: true}, nil
	})
	a, e := accounts.Create(ctx, "测试账号", "abcdef0123456789abcdef0123456789", "test-token", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = accounts.Verify(ctx, a.ID, ""); e != nil {
		t.Fatal(e)
	}
	target := sd.Target{Record: sd.Record{ID: primitive.NewObjectID()}, AccountRef: a.ID, AccountID: a.AccountID, Name: "测试 Worker", WorkerName: "reports", Origin: "https://reports.example.com", Environment: "test", InstallationID: "installation", Status: "ready", ConfigRevision: 1, RemoteVersionID: "old"}
	if _, e = db.Collection("static_delivery_targets").InsertOne(ctx, target); e != nil {
		t.Fatal(e)
	}
	body := `<!doctype html><html><head><title>Report</title></head><body><h1>Published report</h1></body></html>`
	source := httpSource{ex.PageSource{ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(), FullPath: "/report", PublicURL: "https://cms.example/report", SourceSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}, body}
	root := t.TempDir()
	cfg := sd.Config{ProjectRoot: root, Root: filepath.Join(root, "private")}
	exports := ex.NewService(source, nil, ex.Options{ProjectRoot: root, Root: cfg.Root})
	commands := sd.NewCommandService(repo, sd.CommandOptions{Source: source, Exports: exports, Store: sd.NewFileReleaseStore(cfg), Accounts: accounts, Authorize: func(context.Context, sd.ActorSnapshot, bool, bool) error { return nil }})
	remote := &httpRemote{state: sd.RemoteState{Exists: true, SingleVersion: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), VersionID: "old"}}
	verified := false
	runner := commands.NewRunner(sd.RunnerDependencies{Owner: "http-test", Remote: func(context.Context, sd.Job, sd.Target) (sd.Remote, error) { return remote, nil }, Verify: func(_ context.Context, m sd.ReleaseManifest) error { verified = true; return nil }})
	h := &Handlers{Commands: commands, Exports: exports, ActorExtractor: func(*http.Request) (*auth.SessionUser, bool) {
		return &auth.SessionUser{ID: "user", Role: "admin"}, true
	}}
	router := mux.NewRouter()
	Register(router, h)
	call := func(method, path, key string, body any, status int) []byte {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = strings.NewReader(string(b))
		}
		r := httptest.NewRequest(method, path, reader)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s status=%d body=%s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	var accepted struct{ JobID, ExportID primitive.ObjectID }
	// Decode with wire names (ObjectIDs remain JSON strings).
	var response struct {
		JobID    primitive.ObjectID `json:"job_id"`
		ExportID primitive.ObjectID `json:"export_id"`
	}
	_ = json.Unmarshal(call("POST", "/static-exports", "export", map[string]any{"items": []ex.Item{{ContentID: source.page.ContentID}}}, 202), &response)
	accepted.JobID, accepted.ExportID = response.JobID, response.ExportID
	if e = runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	zip := call("GET", "/static-exports/"+accepted.ExportID.Hex()+"/download?format=zip", "", nil, 200)
	if len(zip) < 2 || string(zip[:2]) != "PK" {
		t.Fatal("download is not a ZIP")
	}
	call("GET", "/static-exports/"+accepted.ExportID.Hex()+"/download?format=html", "", nil, 200)
	request := sd.DeployRequest{ExportIDs: []primitive.ObjectID{accepted.ExportID}, ExpectedConfigRevision: 1, ManualScriptReview: true}
	var preview sd.DeploymentPreview
	_ = json.Unmarshal(call("POST", "/static-targets/"+target.ID.Hex()+"/deployment-preview", "", request, 200), &preview)
	if remote.activations != 0 || len(preview.Changes) != 1 {
		t.Fatal("preview activated or omitted report")
	}
	request.ConfirmationDigest = preview.Digest
	deployed := call("POST", "/static-targets/"+target.ID.Hex()+"/deployments", "deploy", request, 202)
	_ = json.Unmarshal(deployed, &response)
	var status struct {
		Job   sd.Job          `json:"job"`
		Pages []sd.PageChange `json:"pages"`
	}
	_ = json.Unmarshal(call("GET", "/static-deployments/"+response.JobID.Hex(), "", nil, 200), &status)
	remote.release = status.Job.ReleaseID.Hex()
	if e = runner.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(call("GET", "/static-deployments/"+response.JobID.Hex(), "", nil, 200), &status)
	if status.Job.State != sd.JobSucceeded || !verified || remote.activations != 1 || len(status.Pages) != 1 || status.Pages[0].URL != "https://reports.example.com/report" {
		t.Fatalf("incomplete chain: %+v", status)
	}
	call("POST", "/static-targets/"+target.ID.Hex()+"/deployments", "deploy", request, 202)
	if remote.activations != 1 {
		t.Fatal("replay reactivated")
	}
	call("GET", "/static-targets/"+target.ID.Hex()+"/deployments?limit=20", "", nil, 200)
}
