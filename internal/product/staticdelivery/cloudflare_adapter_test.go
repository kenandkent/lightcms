package staticdelivery

import (
	"context"
	"encoding/json"
	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

type inspectClient struct {
	CFClient
	versions    int
	versionJSON json.RawMessage
}

type markerClient struct {
	CFClient
	marker string
}

func (f *markerClient) UploadVersion(_ context.Context, _, _ string, _ cf.CompletionToken, _ []cf.Module, marker string) (cf.Version, error) {
	f.marker = marker
	return cf.Version{ID: "new-version"}, nil
}
func TestReviewVersionIdentityIncludesFrozenRelease(t *testing.T) {
	target := releaseTarget()
	releaseID := primitive.NewObjectID()
	routes, _ := json.Marshal(map[string]any{"protocol": 1, "installation_id": target.InstallationID, "target_id": target.ID.Hex(), "release_id": releaseID.Hex()})
	client := &markerClient{}
	remote := &CloudflareRemote{Client: client, Marker: OwnershipMarker{Protocol: 1, InstallationID: target.InstallationID, TargetID: target.ID.Hex()}.String()}
	v, e := remote.UploadVersion(context.Background(), target, "ephemeral", []Module{{Name: "index.js", MIME: "application/javascript+module", Data: []byte("export default {}")}, {Name: "routes.json", MIME: "application/json", Data: routes}})
	if e != nil {
		t.Fatal(e)
	}
	var marker OwnershipMarker
	json.Unmarshal([]byte(client.marker), &marker)
	if marker.ReleaseID != releaseID.Hex() || v.Marker != client.marker {
		t.Fatalf("remote cannot reconcile release: %+v", marker)
	}
}

func (f *inspectClient) Deployments(context.Context, string, string) ([]cf.Deployment, error) {
	d := cf.Deployment{ID: "deployment"}
	raw := `{"id":"deployment","versions":[{"version_id":"version","percentage":100}]}`
	json.Unmarshal([]byte(raw), &d)
	return []cf.Deployment{d}, nil
}
func (f *inspectClient) Version(context.Context, string, string, string) (json.RawMessage, error) {
	f.versions++
	if f.versionJSON != nil {
		return f.versionJSON, nil
	}
	return json.RawMessage(`{"id":"version","resources":{"bindings":[{"type":"plain_text","name":"STATIC_DELIVERY_IDENTITY","text":"{\"p\":1,\"i\":\"installation\",\"t\":\"target\",\"r\":\"release\"}"}],"script":{"etag":"codehash"}}}`), nil
}

func TestAssetsOnlyInspectionPinsIdentityWithoutScript(t *testing.T) {
	f := &inspectClient{versionJSON: json.RawMessage(`{"metadata":{"source":"api"},"annotations":{"workers/tag":"lc1:installation:target:release"},"resources":{"bindings":[],"script":{"etag":"","handlers":null}}}`)}
	state, err := (&CloudflareRemote{Client: f}).Inspect(context.Background(), Target{})
	if err != nil || state.InstallationID != "installation" || state.TargetID != "target" || state.ReleaseID != "release" || state.CodeHash != "assets-only:version" {
		t.Fatalf("assets-only identity missing: %+v %v", state, err)
	}
	f.versionJSON = json.RawMessage(`{"annotations":{"workers/tag":"lc1:installation:target:release"},"resources":{"bindings":[],"script":{"etag":"unexpected-script"}}}`)
	state, err = (&CloudflareRemote{Client: f}).Inspect(context.Background(), Target{})
	if err != nil || state.InstallationID != "" {
		t.Fatal("script upload impersonated an assets-only version", state, err)
	}
}
func (*inspectClient) Settings(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"bindings":[]}`), nil
}
func TestRemoteInspectionReadsVersionMetadata(t *testing.T) {
	f := &inspectClient{}
	r := &CloudflareRemote{Client: f}
	state, e := r.Inspect(context.Background(), Target{})
	if e != nil || state.InstallationID != "installation" || state.TargetID != "target" || state.ReleaseID != "release" || state.VersionID != "version" || state.CodeHash != "codehash" || !state.SingleVersion {
		t.Fatalf("%+v %v", state, e)
	}
}
