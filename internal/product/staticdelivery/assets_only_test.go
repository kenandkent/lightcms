package staticdelivery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAssetsOnlyPreservesAliasesWithoutOverridingPages(t *testing.T) {
	pages := []CapturedPage{}
	for _, p := range []string{"/", "/article", "/article.html", "/中文"} {
		pages = append(pages, releasePage(primitive.NewObjectID(), p))
	}
	r, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: pages})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := nativeAssetConfig(r)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	rules := config["_redirects"].(string)
	for _, want := range []string{"/article/ /article 308\n", "/%E4%B8%AD%E6%96%87.html /%E4%B8%AD%E6%96%87 308\n"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("missing %q in %s", want, rules)
		}
	}
	if strings.Contains(rules, "/article.html /article 308") {
		t.Fatal("explicit page replaced by alias")
	}
}

func TestNewReleaseHealthIsAStaticAsset(t *testing.T) {
	r, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/article")}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := ReleaseUploadFiles(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if f.PublicPath == "/.__lc_delivery/health" {
			var identity map[string]any
			if err = json.Unmarshal(f.Data, &identity); err != nil {
				t.Fatal(err)
			}
			if identity["release_id"] != r.ID.Hex() || identity["target_id"] != r.TargetID.Hex() || f.MIME != "application/json" {
				t.Fatal("invalid static health identity", identity)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("health is still served by JavaScript instead of static assets")
	}
	modules, err := ReleaseModules(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range modules {
		if m.MIME == "application/javascript+module" {
			t.Fatal("new release still contains executable JavaScript")
		}
	}
}

func TestStaticHealthCountsAgainstAssetLimit(t *testing.T) {
	target := releaseTarget()
	target.AssetLimit = 1
	_, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/article")}})
	if CodeOf(err) != CodeAssetLimitExceeded {
		t.Fatal("health asset was not counted against target limit", err)
	}
}
