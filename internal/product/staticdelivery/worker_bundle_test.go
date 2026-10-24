package staticdelivery

import (
	"context"
	"encoding/json"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestReleaseWorkerRoutesNeverAliasOverCanonical(t *testing.T) {
	p := releasePage(primitive.NewObjectID(), "/a")
	q := releasePage(primitive.NewObjectID(), "/a.html")
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{p, q}})
	if e != nil {
		t.Fatal(e)
	}
	modules, e := ReleaseModules(r)
	if e != nil {
		t.Fatal(e)
	}
	var routes struct {
		Pages     map[string]any
		Aliases   map[string]string
		ReleaseID string `json:"release_id"`
	}
	if e = json.Unmarshal(modules[0].Data, &routes); e != nil {
		t.Fatal(e)
	}
	if len(routes.Pages) != 2 || routes.Aliases["/a.html"] != "" || routes.Aliases["/a/"] != "/a" || routes.ReleaseID != r.ID.Hex() {
		t.Fatalf("bad routes %+v", routes)
	}
}
