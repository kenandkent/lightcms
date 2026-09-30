package templatecontract

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	return NewService(db)
}

func uniqueSlug(prefix string) string {
	return prefix + "-" + primitive.NewObjectID().Hex()[:12]
}

func baseInput(slug string) TemplateInput {
	return TemplateInput{
		Slug:         slug,
		Name:         "Test " + slug,
		Category:     "news",
		Status:       "draft",
		HTMLLayout:   "<h1>{{.headline}}</h1><article>{{.body}}</article>",
		ScriptPolicy: "all",
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "markdown"},
		},
	}
}

func intPtr(i int) *int { return &i }

func versionCount(t *testing.T, svc *Service, templateID primitive.ObjectID) int64 {
	t.Helper()
	n, err := svc.db.Collection("template_versions").CountDocuments(
		context.Background(), bson.M{"template_id": templateID})
	if err != nil {
		t.Fatalf("CountDocuments template_versions: %v", err)
	}
	return n
}

// --- slug validation (pure unit, no DB) ---

func TestSlugValidation(t *testing.T) {
	valid := []string{"financial-news", "a", "0abc", "a-b_c9", strings.Repeat("a", 64)}
	for _, s := range valid {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"",                      // empty
		"Financial-news",        // uppercase
		"FINANCIAL-NEWS",        // all uppercase
		"financial news",        // space
		"financial\tnews",       // tab
		"a b",                   // inner space
		strings.Repeat("a", 65), // 65 chars
		"-abc",                  // leading hyphen
		"_abc",                  // leading underscore
		"abc!",                  // illegal char
		"a/b",                   // slash
		"a.b",                   // dot
		"a:b",                   // colon
		"café",                  // non-ASCII
	}
	for _, s := range invalid {
		err := ValidateSlug(s)
		if err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", s)
			continue
		}
		if CodeOf(err) != CodeSlugInvalid {
			t.Errorf("ValidateSlug(%q) code = %q, want %q", s, CodeOf(err), CodeSlugInvalid)
		}
	}
}

func TestSlugDuplicateRejected(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	slug := uniqueSlug("t3dup")

	in := baseInput(slug)
	if _, _, err := svc.Create(ctx, in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	in2 := baseInput(slug)
	in2.Name = "Different Name " + slug
	if _, _, err := svc.Create(ctx, in2); CodeOf(err) != CodeSlugConflict {
		t.Fatalf("second Create code = %q (err=%v), want %q", CodeOf(err), err, CodeSlugConflict)
	}
}

func TestSlugImmutableOnUpdate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	tpl, v1, err := svc.Create(ctx, baseInput(uniqueSlug("t3imm")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mut := baseInput(tpl.Slug)
	mut.Slug = uniqueSlug("t3other")
	mut.HTMLLayout = "<h1>changed</h1>"
	if _, err := svc.Update(ctx, tpl.ID, v1.Version, mut); CodeOf(err) != CodeSlugImmutable {
		t.Fatalf("Update with changed slug code = %q (err=%v), want %q",
			CodeOf(err), err, CodeSlugImmutable)
	}
	if n := versionCount(t, svc, tpl.ID); n != 1 {
		t.Fatalf("version count after rejected slug change = %d, want 1", n)
	}
	cur, err := svc.GetCurrent(ctx, tpl.Slug)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if cur.Version != 1 || cur.HTMLLayout != v1.HTMLLayout {
		t.Fatalf("rejected slug change mutated contract: %+v", cur)
	}
}

// --- versioning: ContractHash decides, same hash = no-op ---

func TestVersionCreateAndGet(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := baseInput(uniqueSlug("t3get"))

	tpl, v1, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tpl.CurrentVersion != 1 || v1.Version != 1 {
		t.Fatalf("expected version 1, got template=%d version=%d", tpl.CurrentVersion, v1.Version)
	}
	if v1.TemplateID != tpl.ID {
		t.Fatalf("version TemplateID %v != template %v", v1.TemplateID, tpl.ID)
	}
	if !strings.HasPrefix(v1.ContractHash, "sha256:") || !strings.HasPrefix(v1.RenderHash, "sha256:") {
		t.Fatalf("hashes not sha256-prefixed: %+v", v1)
	}
	if v1.ContractHash != ContractHash(in) {
		t.Fatalf("stored ContractHash != recomputed input hash")
	}
	if v1.RenderHash != RenderHash(in) {
		t.Fatalf("stored RenderHash != recomputed input hash")
	}

	cur, err := svc.GetCurrent(ctx, in.Slug)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if cur.ID != v1.ID {
		t.Fatalf("GetCurrent returned %v, want %v", cur.ID, v1.ID)
	}
	got, err := svc.GetVersion(ctx, v1.ID)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if got.ID != v1.ID || got.ContractHash != v1.ContractHash {
		t.Fatalf("GetVersion mismatch: %+v vs %+v", got, v1)
	}
}

func TestVersionNoOpOnSameContractHash(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := baseInput(uniqueSlug("t3noop"))

	tpl, v1, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Re-save byte-identical contract: must create NO new version.
	v2, err := svc.Update(ctx, tpl.ID, v1.Version, in)
	if err != nil {
		t.Fatalf("Update same contract: %v", err)
	}
	if v2.Version != 1 {
		t.Fatalf("same-ContractHash re-save produced version %d, want 1", v2.Version)
	}
	if v2.ID != v1.ID {
		t.Fatalf("same-ContractHash re-save returned new doc %v, want %v", v2.ID, v1.ID)
	}
	if n := versionCount(t, svc, tpl.ID); n != 1 {
		t.Fatalf("version count after no-op re-save = %d, want 1", n)
	}
	// Name-only change is display-only (spec §9.3): no new version either,
	// but the mutable display name follows.
	renamed := in
	renamed.Name = "Renamed Display Name"
	v3, err := svc.Update(ctx, tpl.ID, v1.Version, renamed)
	if err != nil {
		t.Fatalf("Update name-only: %v", err)
	}
	if v3.Version != 1 {
		t.Fatalf("name-only change produced version %d, want 1", v3.Version)
	}
	if n := versionCount(t, svc, tpl.ID); n != 1 {
		t.Fatalf("version count after name-only change = %d, want 1", n)
	}
	stored, err := svc.repo.FindTemplateByID(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("FindTemplateByID: %v", err)
	}
	if stored.Name != renamed.Name {
		t.Fatalf("mutable display name = %q, want %q", stored.Name, renamed.Name)
	}
}

func TestVersionBumpOnContractChange(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TemplateInput)
	}{
		{"description", func(in *TemplateInput) { in.Fields[0].Description = "What the headline means" }},
		{"default", func(in *TemplateInput) { in.Fields[0].Default = "Breaking" }},
		{"options", func(in *TemplateInput) {
			in.Fields = append(in.Fields, models.TemplateField{
				Name: "topic", Label: "Topic", Type: "select", Options: "a,b,c",
			})
		}},
		{"validation", func(in *TemplateInput) {
			in.Fields[0].Validation = models.FieldValidation{MaxLength: intPtr(100)}
		}},
		{"html", func(in *TemplateInput) { in.HTMLLayout = "<main>{{.headline}}</main>" }},
		{"policy", func(in *TemplateInput) { in.ScriptPolicy = "none" }},
		{"status", func(in *TemplateInput) { in.Status = "active" }},
		{"category", func(in *TemplateInput) { in.Category = "announcement" }},
		{"label", func(in *TemplateInput) { in.Fields[0].Label = "Headline (new label)" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ctx := context.Background()
			in := baseInput(uniqueSlug("t3bump"))
			tpl, v1, err := svc.Create(ctx, in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			mut := in
			mut.Fields = append([]models.TemplateField(nil), in.Fields...)
			tc.mutate(&mut)
			v2, err := svc.Update(ctx, tpl.ID, v1.Version, mut)
			if err != nil {
				t.Fatalf("Update(%s): %v", tc.name, err)
			}
			if v2.Version != 2 {
				t.Fatalf("Update(%s) produced version %d, want 2", tc.name, v2.Version)
			}
			if v2.ContractHash == v1.ContractHash {
				t.Fatalf("Update(%s) did not change ContractHash", tc.name)
			}
			if n := versionCount(t, svc, tpl.ID); n != 2 {
				t.Fatalf("version count after %s change = %d, want 2", tc.name, n)
			}
		})
	}
}

func TestVersionRenderHashStableOnDescriptionOnly(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := baseInput(uniqueSlug("t3render"))

	tpl, v1, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mut := in
	mut.Fields = append([]models.TemplateField(nil), in.Fields...)
	mut.Fields[0].Description = "Display metadata only"
	mut.Fields[0].Example = "Example headline"
	v2, err := svc.Update(ctx, tpl.ID, v1.Version, mut)
	if err != nil {
		t.Fatalf("Update description-only: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("description-only change must still bump version (ContractHash), got %d", v2.Version)
	}
	if v2.ContractHash == v1.ContractHash {
		t.Fatalf("description-only change must change ContractHash")
	}
	if v2.RenderHash != v1.RenderHash {
		t.Fatalf("description-only change must keep RenderHash stable: %q vs %q",
			v1.RenderHash, v2.RenderHash)
	}
	// HTML change must move RenderHash.
	mut2 := mut
	mut2.HTMLLayout = "<section>{{.headline}}</section>"
	v3, err := svc.Update(ctx, tpl.ID, v2.Version, mut2)
	if err != nil {
		t.Fatalf("Update html: %v", err)
	}
	if v3.RenderHash == v2.RenderHash {
		t.Fatalf("HTML change must change RenderHash")
	}
}

func TestVersionStatusTransitions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := baseInput(uniqueSlug("t3status"))

	tpl, v1, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// draft -> active: new version carrying the new status.
	act := in
	act.Status = "active"
	v2, err := svc.Update(ctx, tpl.ID, v1.Version, act)
	if err != nil {
		t.Fatalf("draft->active: %v", err)
	}
	if v2.Version != 2 || v2.Status != "active" {
		t.Fatalf("draft->active result: %+v", v2)
	}
	// active -> deprecated: allowed.
	dep := act
	dep.Status = "deprecated"
	v3, err := svc.Update(ctx, tpl.ID, v2.Version, dep)
	if err != nil {
		t.Fatalf("active->deprecated: %v", err)
	}
	if v3.Version != 3 || v3.Status != "deprecated" {
		t.Fatalf("active->deprecated result: %+v", v3)
	}
	// deprecated is terminal: reactivation rejected, zero mutation.
	back := dep
	back.Status = "active"
	if _, err := svc.Update(ctx, tpl.ID, v3.Version, back); CodeOf(err) != CodeInputInvalid {
		t.Fatalf("deprecated->active code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
	}
	if n := versionCount(t, svc, tpl.ID); n != 3 {
		t.Fatalf("version count after rejected transition = %d, want 3", n)
	}
	// Unknown status rejected.
	bogus := dep
	bogus.Status = "archived"
	if _, err := svc.Update(ctx, tpl.ID, v3.Version, bogus); CodeOf(err) != CodeInputInvalid {
		t.Fatalf("unknown status code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
	}
}

func TestVersionStaleExpectedVersion(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	tpl, v1, err := svc.Create(ctx, baseInput(uniqueSlug("t3stale")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mut := baseInput(tpl.Slug)
	mut.Name = "whatever"
	mut.HTMLLayout = "<p>new</p>"
	if _, err := svc.Update(ctx, tpl.ID, v1.Version+99, mut); CodeOf(err) != CodeVersionConflict {
		t.Fatalf("stale expectedVersion code = %q (err=%v), want %q",
			CodeOf(err), err, CodeVersionConflict)
	}
	if n := versionCount(t, svc, tpl.ID); n != 1 {
		t.Fatalf("version count after stale update = %d, want 1", n)
	}
}

func TestVersionConcurrentConflict(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := baseInput(uniqueSlug("t3conc"))

	tpl, v1, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inputA := in
	inputA.HTMLLayout = "<h1>A {{.headline}}</h1>"
	inputB := in
	inputB.HTMLLayout = "<h1>B {{.headline}}</h1>"

	type outcome struct {
		ver TemplateVersion
		err error
	}
	out := make([]outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := inputA
			if i == 1 {
				in = inputB
			}
			v, err := svc.Update(ctx, tpl.ID, v1.Version, in)
			out[i] = outcome{ver: v, err: err}
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, o := range out {
		switch {
		case o.err == nil && o.ver.Version == 2:
			wins++
		case CodeOf(o.err) == CodeVersionConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent outcome: ver=%+v err=%v", o.ver, o.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent update: wins=%d conflicts=%d, want 1 and 1 (out=%+v)", wins, conflicts, out)
	}
	if n := versionCount(t, svc, tpl.ID); n != 2 {
		t.Fatalf("version count after concurrent race = %d, want 2 (no duplicate doc)", n)
	}
	cur, err := svc.GetCurrent(ctx, tpl.Slug)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if cur.Version != 2 {
		t.Fatalf("current version = %d, want 2", cur.Version)
	}
}

func TestVersionGetNotFound(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.GetCurrent(ctx, uniqueSlug("t3missing")); CodeOf(err) != CodeNotFound {
		t.Fatalf("GetCurrent missing code = %q (err=%v), want %q", CodeOf(err), err, CodeNotFound)
	}
	if _, err := svc.GetVersion(ctx, primitive.NewObjectID()); CodeOf(err) != CodeVersionNotFound {
		t.Fatalf("GetVersion missing code = %q (err=%v), want %q", CodeOf(err), err, CodeVersionNotFound)
	}
	if _, err := svc.Update(ctx, primitive.NewObjectID(), 1, baseInput(uniqueSlug("t3x"))); CodeOf(err) != CodeNotFound {
		t.Fatalf("Update missing template code = %q (err=%v), want %q", CodeOf(err), err, CodeNotFound)
	}
}

func TestVersionCreateInputValidation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	base := func() TemplateInput { return baseInput(uniqueSlug("t3val")) }

	t.Run("empty name", func(t *testing.T) {
		in := base()
		in.Name = ""
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
		}
	})
	t.Run("bad status", func(t *testing.T) {
		in := base()
		in.Status = "archived"
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
		}
	})
	t.Run("bad field name", func(t *testing.T) {
		for _, name := range []string{"Title", "1abc", "id", "publication_id", "has space"} {
			in := base()
			in.Fields = []models.TemplateField{{Name: name, Label: "X", Type: "text"}}
			if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
				t.Fatalf("field %q: code = %q (err=%v), want %q", name, CodeOf(err), err, CodeInputInvalid)
			}
		}
	})
	t.Run("duplicate field names", func(t *testing.T) {
		in := base()
		in.Fields = []models.TemplateField{
			{Name: "headline", Label: "A", Type: "text"},
			{Name: "headline", Label: "B", Type: "text"},
		}
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
		}
	})
	t.Run("unknown field type", func(t *testing.T) {
		in := base()
		in.Fields = []models.TemplateField{{Name: "grid", Label: "Grid", Type: "group"}}
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
		}
	})
	t.Run("bad script policy", func(t *testing.T) {
		in := base()
		in.ScriptPolicy = "sometimes"
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeInputInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeInputInvalid)
		}
	})
	t.Run("bad slug", func(t *testing.T) {
		in := base()
		in.Slug = "Has Space"
		if _, _, err := svc.Create(ctx, in); CodeOf(err) != CodeSlugInvalid {
			t.Fatalf("code = %q (err=%v), want %q", CodeOf(err), err, CodeSlugInvalid)
		}
	})
}
