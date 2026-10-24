package generation_test

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/generation"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson"
	"testing"
)

func TestDurableUpgradeRestartsWithFrozenBaselineAndNoDuplicatePublication(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v := s.seedTemplate(t, "durable-upgrade", "", nil)
	actor := adminActor()
	req := generation.GenerateRequest{Template: "durable-upgrade", Title: "Page", Slug: "durable-page", Mode: "publish", ExpectedTemplateVersion: &v, Data: map[string]any{"headline": "Frozen"}}
	if _, err := s.gen.Generate(pubCtx(actor, "durable-page", req, v), actor, req); err != nil {
		t.Fatal(err)
	}
	tpl, err := templatecontract.NewRepository(s.db).FindTemplateBySlug(ctx, "durable-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.tpls.Update(ctx, tpl.ID, v, templatecontract.TemplateInput{Slug: tpl.Slug, Name: tpl.Name, Category: "news", Status: "active", HTMLLayout: genLayout + "<!-- upgrade -->", Fields: []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text", Required: true}}}); err != nil {
		t.Fatal(err)
	}
	upgrade, err := s.gen.StartUpgradeJob(ctx, actor, tpl.Slug)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context, generation.Actor) error { return nil }
	first := jobs.NewRunner(jobs.NewRepository(s.db, 10), 1)
	s.gen.EnableDurableUpgrades(first, allow)
	queued, err := s.gen.SubmitUpgradeRun(ctx, actor, upgrade.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate executor process after submission, without changing frozen
	// publication or target-template baselines.
	fresh := jobs.NewRunner(jobs.NewRepository(s.db, 10), 1)
	s.gen.EnableDurableUpgrades(fresh, allow)
	if err := fresh.RunOnce(ctx, "template_upgrade"); err != nil {
		t.Fatal(err)
	}
	done, err := s.gen.GetUpgradeJob(ctx, actor, upgrade.ID)
	if err != nil || done.Status != generation.UpgradeJobCompleted {
		t.Fatalf("upgrade did not finish: %+v %v", done, err)
	}
	before := countDocs(t, s.db, "content_publications", bson.M{})
	replay, err := s.gen.SubmitUpgradeRun(ctx, actor, upgrade.ID)
	if err != nil || replay.ID != queued.ID {
		t.Fatalf("restart changed workflow: %+v %v", replay, err)
	}
	if err := fresh.RunOnce(ctx, "template_upgrade"); err != nil {
		t.Fatal(err)
	}
	if after := countDocs(t, s.db, "content_publications", bson.M{}); after != before {
		t.Fatalf("replay minted a publication: %d -> %d", before, after)
	}
}
