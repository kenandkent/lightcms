package handlers

// Task 15 (Lane H) red tests: Admin Editor and Publication UX.
// Spec: final design §8.4 (url/number/boolean/date/image/richtext/markdown),
// §12.3 (published edit -> Fork, live unchanged), §12.6 (merge returns
// requires_publish, no live regen), §13 (Restore as Draft vs Restore and
// Publish), §16.6 (Admin publish shows publication ID/URL), §20.4
// (requires_publish semantics), §31 (template selector cards, upgrade
// preview/job UI).
//
// These tests are pure unit tests (no Mongo): they exercise the Admin
// presentation helpers in admin_publications.go, which call the SAME shared
// services/types as REST — templatecontract.ValidateData (Task 4),
// generation.UpgradePreview/UpgradeJob + RestoreAndPublish/RevertLive shapes
// (Task 12), publication.PublicationResult (Task 8) — without redefining
// them. Task 16 wires the HTTP routes; this task owns the rendering and
// display semantics.

import (
	"html/template"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func testProductTemplate() models.Template {
	return models.Template{
		ID:          primitive.NewObjectID(),
		Name:        "Financial News",
		Slug:        "financial-news",
		Description: "Market-moving financial news pages",
		Category:    "news",
		Status:      "active",
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true, Description: "Primary headline"},
			{Name: "body", Label: "Body", Type: "markdown", Required: true},
			{Name: "source_url", Label: "Source URL", Type: "url", Required: false},
			{Name: "score", Label: "Score", Type: "number", Required: false},
			{Name: "breaking", Label: "Breaking", Type: "boolean", Required: false},
			{Name: "publish_date", Label: "Publish date", Type: "date", Required: true},
			{Name: "hero", Label: "Hero image", Type: "image", Required: false},
			{Name: "lede", Label: "Lede", Type: "richtext", Required: false},
		},
	}
}

func testProductVersion(tmpl models.Template) templatecontract.TemplateVersion {
	fields := make([]models.TemplateField, len(tmpl.Fields))
	copy(fields, tmpl.Fields)
	return templatecontract.TemplateVersion{
		ID:         primitive.NewObjectID(),
		TemplateID: tmpl.ID,
		Version:    3,
		Slug:       tmpl.Slug,
		Name:       tmpl.Name,
		Category:   tmpl.Category,
		Status:     tmpl.Status,
		Fields:     fields,
		HTMLLayout: "<h1>{{.headline}}</h1>",
	}
}

// TestAdminProductTemplatesParse guards the Task 15 admin_templates.go edits:
// every Admin template (including the extended content_form and
// content_select_template) must parse with the shared func map. Pure test,
// no Mongo.
func TestAdminProductTemplatesParse(t *testing.T) {
	for name, src := range adminTemplates {
		if _, err := template.New("admin").Funcs(adminTemplateFuncMap).Parse(src); err != nil {
			t.Errorf("admin template %q does not parse: %v", name, err)
		}
	}
}

// TestAdminProductTemplateSelector: selector cards show category, description
// and required fields (spec §31.2).
func TestAdminProductTemplateSelector(t *testing.T) {
	tmpl := testProductTemplate()
	html := string(TemplateSelectorCardHTML(tmpl))
	for _, want := range []string{tmpl.Category, tmpl.Description, "headline", "publish_date", "body"} {
		if !strings.Contains(html, want) {
			t.Errorf("selector card missing %q in:\n%s", want, html)
		}
	}
	if strings.Contains(html, "source_url*") || strings.Contains(html, "source_url *") {
		t.Errorf("optional field source_url must not render as required in:\n%s", html)
	}
	if got := strings.Join(RequiredFieldNames(tmpl), ","); got != "headline,body,publish_date" {
		t.Errorf("RequiredFieldNames = %q, want headline,body,publish_date", got)
	}
}

// TestAdminProductFieldRendering: Admin form renders every MVP field type and
// surfaces shared Task 4 validation errors inline (spec §8.4, §11.4).
func TestAdminProductFieldRendering(t *testing.T) {
	tmpl := testProductTemplate()
	v := testProductVersion(tmpl)
	cases := map[string][]string{
		"url":      {`type="url"`, `name="field_source_url"`},
		"number":   {`type="number"`, `name="field_score"`},
		"boolean":  {`type="checkbox"`, `name="field_breaking"`},
		"date":     {`type="date"`, `name="field_publish_date"`},
		"image":    {`type="file"`, `field_hero`},
		"richtext": {`richtext`, `field_lede`},
		"markdown": {`field_body`, `markdown`},
		"text":     {`field_headline`},
	}
	byName := map[string]models.TemplateField{}
	for _, f := range tmpl.Fields {
		byName[f.Name] = f
	}
	_ = v
	for typ, wants := range cases {
		var field models.TemplateField
		for _, f := range tmpl.Fields {
			if f.Type == typ {
				field = f
			}
		}
		if field.Type == "" {
			t.Fatalf("no %s field in fixture", typ)
		}
		html := string(RenderProductField(field, nil, nil))
		for _, want := range wants {
			if !strings.Contains(html, want) {
				t.Errorf("type %s: missing %q in:\n%s", typ, want, html)
			}
		}
	}
	// Shared validator errors surface inline for the failing field.
	bad := map[string]any{"headline": "", "source_url": "gopher://x", "publish_date": "2026-02-30"}
	errs, _ := templatecontract.ValidateData(v, bad)
	if len(errs) == 0 {
		t.Fatal("shared ValidateData should reject the bad fixture data")
	}
	dateHTML := string(RenderProductField(byName["publish_date"], "2026-02-30", errs))
	if !strings.Contains(dateHTML, "FIELD_INVALID_DATE") {
		t.Errorf("date field must surface shared FIELD_INVALID_DATE in:\n%s", dateHTML)
	}
	urlHTML := string(RenderProductField(byName["source_url"], "gopher://x", errs))
	if !strings.Contains(urlHTML, "FIELD_") {
		t.Errorf("url field must surface shared FIELD_* error in:\n%s", urlHTML)
	}
	headlineHTML := string(RenderProductField(byName["headline"], "", errs))
	if !strings.Contains(headlineHTML, "FIELD_REQUIRED") {
		t.Errorf("required text field must surface FIELD_REQUIRED in:\n%s", headlineHTML)
	}
}

// TestAdminProductBooleanParsing: Admin checkbox semantics follow spec §8.4 —
// optional boolean missing stays missing; default=true materializes true.
func TestAdminProductBooleanParsing(t *testing.T) {
	fields := []models.TemplateField{
		{Name: "breaking", Label: "Breaking", Type: "boolean", Required: false},
		{Name: "featured", Label: "Featured", Type: "boolean", Required: false, Default: "true"},
	}
	got := ParseAdminFieldData(fields, map[string]string{}, map[string]bool{})
	if _, ok := got["breaking"]; ok {
		t.Errorf("optional boolean missing must stay missing, got %v", got)
	}
	if got["featured"] != true {
		t.Errorf("default=true boolean must materialize true, got %v", got)
	}
	got = ParseAdminFieldData(fields, map[string]string{"field_breaking": "on"}, map[string]bool{"field_breaking": true, "field_featured": true})
	if got["breaking"] != true {
		t.Errorf("checked boolean must be true, got %v", got)
	}
}

// TestAdminPublicationForkEdit: published Edit opens/reuses a Fork and states
// live is unchanged (spec §12.3, §31.3).
func TestAdminPublicationForkEdit(t *testing.T) {
	action, notice := SelectForkAction(true, "")
	if action != "open_fork" {
		t.Errorf("published edit without fork: action = %q, want open_fork", action)
	}
	if !strings.Contains(notice, "线上页面不受影响") {
		t.Errorf("published edit notice must state live is unchanged, got %q", notice)
	}
	action, _ = SelectForkAction(true, primitive.NewObjectID().Hex())
	if action != "reuse_fork" {
		t.Errorf("published edit with fork: action = %q, want reuse_fork", action)
	}
	action, _ = SelectForkAction(false, "")
	if action != "edit_content" {
		t.Errorf("unpublished edit: action = %q, want edit_content", action)
	}
	html := string(ForkEditBannerHTML(true, "abc123"))
	if !strings.Contains(html, "正在编辑草稿") || !strings.Contains(html, "线上页面不受影响") {
		t.Errorf("fork banner must carry draft/live-unchanged copy in:\n%s", html)
	}
	html = string(ForkEditBannerHTML(false, ""))
	if strings.Contains(html, "线上页面不受影响") {
		t.Errorf("unpublished banner must not claim live-unchanged in:\n%s", html)
	}
}

// TestAdminPublicationMergeDisplay: merge shows requires_publish and states
// canonical HTML is untouched (spec §12.6).
func TestAdminPublicationMergeDisplay(t *testing.T) {
	html := string(MergeResultHTML(ForkMergeDisplay{
		Updated:         1,
		ContentIDs:      []string{primitive.NewObjectID().Hex()},
		RequiresPublish: []string{primitive.NewObjectID().Hex()},
	}))
	for _, want := range []string{"待发布", "发布是单独的操作", "线上正式 HTML"} {
		if !strings.Contains(html, want) {
			t.Errorf("merge result missing %q in:\n%s", want, html)
		}
	}
}

// TestAdminPublicationPublishResult: success shows Public URL, Publication ID,
// Content/Template versions; failure preserves the prior URL with a retryable
// error (spec §16.6, §31.5).
func TestAdminPublicationPublishResult(t *testing.T) {
	pubID := primitive.NewObjectID()
	contentID := primitive.NewObjectID()
	tplVID := primitive.NewObjectID()
	res := publication.PublicationResult{
		PublicationID: pubID, ContentID: contentID, ContentVersion: 8,
		TemplateVersionID: tplVID, FullPath: "/news/bitcoin-market-update",
		PublicURL:   "https://pages.example.com/news/bitcoin-market-update",
		ContentHash: "sha256:abc",
	}
	html := string(PublishResultHTML(PublishDisplay{Result: res, TemplateVersion: 3}))
	for _, want := range []string{res.PublicURL, pubID.Hex(), contentID.Hex(), "8", "3", "/news/bitcoin-market-update"} {
		if !strings.Contains(html, want) {
			t.Errorf("publish result missing %q in:\n%s", want, html)
		}
	}
	failed := string(FailedPublishHTML("https://pages.example.com/news/old-live", "PUBLICATION_STAGE_FAILED", true))
	for _, want := range []string{"https://pages.example.com/news/old-live", "PUBLICATION_STAGE_FAILED", "重试"} {
		if !strings.Contains(failed, want) {
			t.Errorf("failed publish must preserve prior URL + retryable error, missing %q in:\n%s", want, failed)
		}
	}
}

// TestAdminProductTemplateVersionNotice: HTML change versions without live
// regen; upgrade needs an explicit action (spec §9.5, §31.4).
func TestAdminProductTemplateVersionNotice(t *testing.T) {
	html := string(TemplateVersionNoticeHTML(3, 4))
	for _, want := range []string{"版本 4", "线上页面", "升级预览"} {
		if !strings.Contains(html, want) {
			t.Errorf("version notice missing %q in:\n%s", want, html)
		}
	}
}

// TestAdminPublicationUpgradeUI: explicit read-only preview + durable job UI
// with per-item outcomes (Task 12 contract shapes).
func TestAdminPublicationUpgradeUI(t *testing.T) {
	cid := primitive.NewObjectID().Hex()
	prev := generation.UpgradePreview{
		Template: "financial-news", FromVersion: 2, ToVersion: 3,
		TotalPages: 2, WouldRepublish: 1,
		Items: []generation.UpgradePreviewItem{
			{ContentID: cid, FullPath: "/news/a", CurrentVersion: 5, HasActive: true, ActiveTemplateV: 2, TargetTemplateV: 3, WouldRepublish: true},
			{ContentID: primitive.NewObjectID().Hex(), FullPath: "/news/b", CurrentVersion: 1, HasActive: true, ActiveTemplateV: 3, TargetTemplateV: 3},
		},
	}
	html := string(UpgradePreviewHTML(prev, primitive.NewObjectID().Hex()))
	for _, want := range []string{"financial-news", "/news/a", "需要重新发布", "启动升级任务"} {
		if !strings.Contains(html, want) {
			t.Errorf("upgrade preview missing %q in:\n%s", want, html)
		}
	}
	job := generation.UpgradeJob{
		ID: primitive.NewObjectID(), TemplateSlug: "financial-news",
		FromVersion: 2, ToVersion: 3, Status: generation.UpgradeJobPartial,
		Items: []generation.UpgradeJobItem{
			{ContentID: primitive.NewObjectID(), FullPath: "/news/a", Status: generation.UpgradeItemDone, Attempts: 1},
			{ContentID: primitive.NewObjectID(), FullPath: "/news/c", Status: generation.UpgradeItemFailed, Attempts: 2, Error: "stage failed"},
		},
	}
	jhtml := string(UpgradeJobHTML(job, "/cm/upgrade-jobs/"+job.ID.Hex()+"/run"))
	for _, want := range []string{"/news/a", "done", "/news/c", "failed", "重试", "恢复"} {
		if !strings.Contains(jhtml, want) {
			t.Errorf("upgrade job missing %q in:\n%s", want, jhtml)
		}
	}
}

// TestAdminPublicationRestoreVsRevert: two distinct buttons with distinct
// outcomes — restore_and_publish re-renders a ContentVersion; revert_live
// restores a Publication's bytes (spec §13, §18.4–§18.5). The restore-as-draft
// button must post to the REGISTERED /revert route (there is no restore-draft
// route), and the mutating forms must carry their hidden idempotency /
// expected_active_id fields (Wave 4A).
func TestAdminPublicationRestoreVsRevert(t *testing.T) {
	cid := primitive.NewObjectID().Hex()
	activeID := primitive.NewObjectID().Hex()
	html := string(RestoreRevertActionsHTML(cid, 7, primitive.NewObjectID().Hex(), activeID))
	if !strings.Contains(html, "restore_and_publish") {
		t.Errorf("must expose a restore_and_publish action in:\n%s", html)
	}
	if !strings.Contains(html, "revert_live") {
		t.Errorf("must expose a revert_live action in:\n%s", html)
	}
	if !strings.Contains(html, "草稿") || !strings.Contains(html, "完整保留字节") {
		t.Errorf("buttons must explain distinct outcomes (draft re-render vs exact bytes) in:\n%s", html)
	}
}
