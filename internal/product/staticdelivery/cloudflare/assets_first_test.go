package cloudflare

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func TestNativeAssetConfigIsSentAsVersionMetadata(t *testing.T) {
	config := []byte(`{"run_worker_first":["/.__lc_delivery/health"],"html_handling":"none","not_found_handling":"none","_headers":"/*\n  X-LC-Release: release\n","_redirects":"/ /.__lc_delivery/root.html 200\n"}`)
	body, kind, e := moduleBody("completion", []Module{{Name: "index.js", MIME: "application/javascript+module", Data: []byte("export default {}")}, {Name: "delivery-assets-config.json", MIME: "application/json", Data: config}}, "marker")
	if e != nil {
		t.Fatal(e)
	}
	_, p, _ := mime.ParseMediaType(kind)
	reader := multipart.NewReader(bytes.NewReader(body), p["boundary"])
	var metadata struct {
		Assets struct{ Config map[string]any }
	}
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if part.FormName() == "delivery-assets-config.json" {
			t.Fatal("metadata-only configuration was uploaded as a Worker module")
		}
		if part.FormName() == "metadata" {
			if e = json.NewDecoder(part).Decode(&metadata); e != nil {
				t.Fatal(e)
			}
		}
	}
	got := metadata.Assets.Config
	if _, ok := got["run_worker_first"].([]any); !ok || got["_headers"] == nil || got["_redirects"] == nil {
		t.Fatalf("asset-first config not applied to upload metadata: %+v", got)
	}
}
