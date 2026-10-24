package services

import (
	"context"
	"encoding/json"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/services/importer"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDurableRSSRestartsAndFetchesFeedOnlyOnce(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	requests := 0
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title><link>https://example.test</link><description>Feed</description><item><title>Frozen feed page</title><link>https://example.test/one</link><description>Body</description></item></channel></rss>`))
	}))
	defer feed.Close()
	cs := NewContentService(db)
	s := NewImportService(db, cs)
	allow := func(context.Context, jobs.Job) error { return nil }
	s.EnableDurable(jobs.NewRunner(jobs.NewRepository(db, 10), 1), allow)
	source := &models.ImportSource{Name: "Feed", URL: feed.URL, FolderPath: "/feed", Active: true}
	if err := s.CreateSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	job, err := s.RunRSSImport(ctx, source.ID, "editor")
	if err != nil {
		t.Fatal(err)
	}
	freshRunner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	fresh := NewImportService(db, cs)
	fresh.EnableDurable(freshRunner, allow)
	if err := freshRunner.RunOnce(ctx, "import"); err != nil {
		t.Fatal(err)
	}
	finished, err := fresh.GetJob(ctx, job.ID)
	if err != nil || finished.Status != models.ImportStatusDone || finished.Created != 1 {
		t.Fatalf("RSS did not resume: %+v %v", finished, err)
	}
	if err := freshRunner.RunOnce(ctx, "import"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("completed RSS fetched again: %d", requests)
	}
}

func TestDurableMarkdownFreezesTemplateAndPublishIntent(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	tid := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "templates", bson.M{"_id": tid, "name": "Frozen template", "current_version": 2}); err != nil {
		t.Fatal(err)
	}
	s := NewImportService(db, NewContentService(db))
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
	job, err := s.RunMarkdownImport(ctx, []importer.MarkdownPage{{Filename: "page.md", Frontmatter: map[string]string{"title": "Page", "template": "Frozen template", "published": "true"}, Body: "Body"}}, "", "/imports", false, "editor")
	if err != nil {
		t.Fatal(err)
	}
	items, err := runner.Repo.Items(ctx, *job.WorkflowID, "", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("missing input %v", err)
	}
	var plan importPlan
	if err = json.Unmarshal([]byte(items[0].Input["plan"].(string)), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.TemplateID != tid || !plan.AutoPublish {
		t.Fatalf("template or frontmatter publication intent not frozen: %+v", plan)
	}
}

func TestDurableImportCrashAfterDraftCommitDoesNotEditAgain(t *testing.T) {
	for _, format := range []string{"csv", "markdown"} {
		t.Run(format, func(t *testing.T) {
			db, cleanup := testutil.MustConnectTestDB(t)
			defer cleanup()
			ctx := context.Background()
			cs := NewContentService(db)
			s := NewImportService(db, cs)
			repo := jobs.NewRepository(db, 10)
			runner := jobs.NewRunner(repo, 1)
			allow := func(context.Context, jobs.Job) error { return nil }
			s.EnableDurable(runner, allow)
			var job *models.ImportJob
			var err error
			if format == "csv" {
				job, err = s.RunCSVImport(ctx, []importer.CSVRecord{{Row: 1, Fields: map[string]string{"title": "Checkpoint", "slug": "checkpoint"}}}, nil, "title", "slug", primitive.NewObjectID(), "/imports", false, "editor")
			} else {
				job, err = s.RunMarkdownImport(ctx, []importer.MarkdownPage{{Filename: "checkpoint.md", Frontmatter: map[string]string{"title": "Checkpoint", "slug": "checkpoint"}, Body: "Frozen body"}}, "", "/imports", false, "editor")
			}
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := repo.Claim(ctx, "import", "crashed-process", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			items, err := repo.Items(ctx, claimed.ID, "", 1)
			if err != nil {
				t.Fatal(err)
			}
			var plan importPlan
			if err := json.Unmarshal([]byte(items[0].Input["plan"].(string)), &plan); err != nil {
				t.Fatal(err)
			}
			state := &durableImportState{Job: *claimed, Item: items[0], Plan: plan}
			work := context.WithValue(ctx, durableImportKey{}, state)
			if format == "csv" {
				s.processCSV(work, job, plan.Records, plan.ColumnMap, plan.TitleColumn, plan.SlugColumn, plan.TemplateID, plan.Folder, false)
			} else {
				s.processMarkdown(work, job, plan.Pages, plan.Template, plan.Folder, false)
			}
			if state.Written["content_id"] == nil {
				t.Fatal("draft stage was not committed")
			}
			// Crash before CommitItem/finalization; the next process takes over the
			// expired lease and must use the draft checkpoint, not edit it again.
			if err := db.UpdateOne(ctx, "workflow_jobs", bson.M{"_id": claimed.ID}, bson.M{"$set": bson.M{"lease_until": time.Now().Add(-time.Minute)}}); err != nil {
				t.Fatal(err)
			}
			freshRunner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
			fresh := NewImportService(db, cs)
			fresh.EnableDurable(freshRunner, allow)
			if err := freshRunner.RunOnce(ctx, "import"); err != nil {
				t.Fatal(err)
			}
			cid, _ := primitive.ObjectIDFromHex(state.Written["content_id"].(string))
			var content models.Content
			if err := db.FindOne(ctx, "content", bson.M{"_id": cid}, &content); err != nil {
				t.Fatal(err)
			}
			if content.CurrentVersion != 1 {
				t.Fatalf("restart edited committed draft: version %d", content.CurrentVersion)
			}
			if count, err := db.Count(ctx, "content_versions", bson.M{"content_id": cid}); err != nil || count != 1 {
				t.Fatalf("duplicate history: %d %v", count, err)
			}
			finished, err := fresh.GetJob(ctx, job.ID)
			if err != nil || finished.Status != models.ImportStatusDone || finished.Created != 1 {
				t.Fatalf("lost checkpoint: %+v %v", finished, err)
			}
		})
	}
}

func TestDurableCSVUsesFrozenRowsAfterRestart(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	repo := jobs.NewRepository(db, 10)
	runner := jobs.NewRunner(repo, 1)
	cs := NewContentService(db)
	s := NewImportService(db, cs)
	allow := func(context.Context, jobs.Job) error { return nil }
	s.EnableDurable(runner, allow)
	rows := []importer.CSVRecord{{Row: 1, Fields: map[string]string{"title": "Frozen", "slug": "frozen", "body": "Original"}}, {Row: 2, Fields: map[string]string{"title": "Second", "slug": "second", "body": "Second"}}}
	j, err := s.RunCSVImport(ctx, rows, map[string]string{"body": "body"}, "title", "slug", primitive.NewObjectID(), "/imports", false, "editor")
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Fields["title"] = "Changed after submission"
	if n, err := db.Count(ctx, "content", bson.M{}); err != nil || n != 0 {
		t.Fatalf("submission executed in memory: %d %v", n, err)
	}
	freshRunner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	fresh := NewImportService(db, cs)
	fresh.EnableDurable(freshRunner, allow)
	if err := freshRunner.RunOnce(ctx, "import"); err != nil {
		t.Fatal(err)
	}
	var c models.Content
	if err := db.FindOne(ctx, "content", bson.M{"full_path": "/imports/frozen"}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Title != "Frozen" || c.CurrentVersion != 1 {
		t.Fatalf("wrong restored input: %+v", c)
	}
	stored, err := fresh.GetJob(ctx, j.ID)
	if err != nil || stored.Status != models.ImportStatusDone || stored.Created != 2 {
		t.Fatalf("legacy progress missing: %+v %v", stored, err)
	}
	logs, err := fresh.GetJobLogs(ctx, j.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(logs); i++ {
		if logs[i].Seq <= logs[i-1].Seq {
			t.Fatalf("durable log sequence repeated after item restart: %+v", logs)
		}
	}
}

func TestFrozenPublicationRequiresIdempotencyWiring(t *testing.T) {
	old := internalIdem
	internalIdem = nil
	defer func() { internalIdem = old }()
	// Refuse before reading mutable content or delegating to the legacy path.
	err := (&ContentService{}).publishInternalVersion(context.Background(), primitive.NewObjectID(), 3, "import", "/import", "stable")
	if err == nil || err.Error() != "frozen publication requires idempotency wiring" {
		t.Fatalf("unexpected frozen publication fallback: %v", err)
	}
}
