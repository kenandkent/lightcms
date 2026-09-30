package migration

// Task 17B coverage-gap tests: unexported pure helpers + sortReport tiebreaks.
// In-package so unexported identifiers are reachable; no DB touched.

import (
	"errors"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestCoverGapPureHelpers(t *testing.T) {
	if !isDupKey(mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000}}}) {
		t.Fatalf("isDupKey(11000)")
	}
	if !isDupKey(errors.New("E11000 duplicate key")) {
		t.Fatalf("isDupKey(E11000 string)")
	}
	if !isDupKey(errors.New("duplicate key error")) {
		t.Fatalf("isDupKey(duplicate key string)")
	}
	if isDupKey(nil) || isDupKey(errors.New("timeout")) {
		t.Fatalf("isDupKey negative")
	}

	if withPrefix("sha256:abc") != "sha256:abc" {
		t.Fatalf("withPrefix passthrough")
	}
	if withPrefix("SHA256:ABC") != "SHA256:ABC" {
		t.Fatalf("withPrefix uppercase passthrough")
	}
	if withPrefix("abc") != "sha256:abc" {
		t.Fatalf("withPrefix add")
	}

	if toInt64(int64(7)) != 7 || toInt64(int32(7)) != 7 || toInt64(int(7)) != 7 ||
		toInt64(float64(7.9)) != 7 || toInt64("7") != 0 || toInt64(nil) != 0 {
		t.Fatalf("toInt64 branches")
	}

	if stripHash("  SHA256:ABCDEF  ") != "sha256:abcdef" {
		t.Fatalf("stripHash = %q", stripHash("  SHA256:ABCDEF  "))
	}
	if stripHash("rawhex") != "rawhex" {
		t.Fatalf("stripHash raw")
	}
	if shaHex([]byte("x")) == "" {
		t.Fatalf("shaHex empty")
	}

	m := &Migrator{base: "https://example.com/"}
	if got := m.publicURL("/a"); got != "https://example.com/a" {
		t.Fatalf("publicURL base = %q", got)
	}
	bare := &Migrator{}
	if got := bare.publicURL("/a"); got != "/a" {
		t.Fatalf("publicURL bare = %q", got)
	}

	v1 := buildV1(models.Template{
		ID: primitive.NewObjectID(), Name: "T", Slug: "t",
		HTMLLayout: "<p></p>",
		Fields:     []models.TemplateField{{Name: "h", Type: "text"}},
	}, time.Now())
	if v1.Version != 1 || v1.ContractHash == "" || v1.RenderHash == "" {
		t.Fatalf("buildV1 = %+v", v1)
	}

	p := &page{}
	if contentVersionOf(p) != 1 {
		t.Fatalf("genesis contentVersionOf")
	}
	p.maxVer = 4
	if contentVersionOf(p) != 4 {
		t.Fatalf("maxVer contentVersionOf")
	}
	p.content.CurrentVersion = 9
	if contentVersionOf(p) != 9 {
		t.Fatalf("current contentVersionOf")
	}
	if serving(&page{}) || !serving(&page{content: models.Content{Published: true}}) {
		t.Fatalf("serving branches")
	}
	if reconcilable(&page{content: models.Content{Published: true}, invalid: "x"}) {
		t.Fatalf("reconcilable invalid")
	}
}

func TestCoverGapSortReportTiebreaks(t *testing.T) {
	a := &analysis{}
	a.rep.InvalidSlugs = []SlugIssue{{TemplateID: "b"}, {TemplateID: "a"}, {TemplateID: "a", Reason: "z"}, {TemplateID: "a", Reason: "a"}}
	a.rep.InvalidPaths = []PageItem{
		{ContentID: "c2", FullPath: "/same"},
		{ContentID: "c1", FullPath: "/same"},
		{ContentID: "c0", FullPath: "/aaa"},
	}
	a.rep.MissingFiles = []PageItem{{ContentID: "c1", FullPath: "/same"}, {ContentID: "c0", FullPath: "/same"}}
	a.rep.RenderErrors = []PageItem{{ContentID: "c1", FullPath: "/same"}, {ContentID: "c0", FullPath: "/same"}}
	a.rep.Verified = []PageItem{{ContentID: "c1", FullPath: "/same"}, {ContentID: "c0", FullPath: "/same"}}
	a.rep.LegacyUnverified = []PageItem{{ContentID: "c1", FullPath: "/same"}, {ContentID: "c0", FullPath: "/same"}}
	a.rep.SkippedActive = []PageItem{{ContentID: "c1", FullPath: "/same"}, {ContentID: "c0", FullPath: "/same"}}
	a.rep.VersionDuplicates = []VersionDuplicate{
		{ContentID: "c2", Version: 2}, {ContentID: "c1", Version: 2}, {ContentID: "c1", Version: 1},
	}
	sortReport(a)
	if a.rep.InvalidPaths[0].ContentID != "c0" || a.rep.InvalidPaths[1].ContentID != "c1" {
		t.Fatalf("InvalidPaths tiebreak: %+v", a.rep.InvalidPaths)
	}
	// VersionDuplicates are ordered by the analyze pass, not sortReport;
	// sortReport must leave them untouched.
	if len(a.rep.VersionDuplicates) != 3 || a.rep.VersionDuplicates[0].ContentID != "c2" {
		t.Fatalf("VersionDuplicates touched: %+v", a.rep.VersionDuplicates)
	}
}
