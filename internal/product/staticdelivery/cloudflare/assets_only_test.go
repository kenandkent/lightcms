package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"testing"
)

func TestAssetsOnlyUploadHasNoExecutableParts(t *testing.T) {
	config := []byte(`{"html_handling":"none","not_found_handling":"none","_headers":"/*\n  X-LC-Release: release\n"}`)
	body, kind, err := moduleBody("completion", []Module{{Name: "delivery-assets-only.json", MIME: "application/json", Data: config}}, `{"p":1,"i":"installation","t":"target","r":"release"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(kind)
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err = json.NewDecoder(part).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if part.FormName() != "metadata" || metadata["main_module"] != nil {
		t.Fatal("assets-only upload contains executable entrypoint", metadata)
	}
	if len(metadata["bindings"].([]any)) != 0 {
		t.Fatal("assets-only upload contains runtime bindings")
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations["workers/tag"] != "lc1:installation:target:release" {
		t.Fatal("ownership identity missing", annotations)
	}
	if _, err = reader.NextPart(); err != io.EOF {
		t.Fatal("assets-only upload contains module parts", err)
	}
}

func TestAssetsOnlyRejectsRuntimeRoutingAndModules(t *testing.T) {
	marker := `{"p":1,"i":"installation","t":"target","r":"release"}`
	config := Module{Name: "delivery-assets-only.json", MIME: "application/json", Data: []byte(`{"html_handling":"none","not_found_handling":"none"}`)}
	for _, modules := range [][]Module{
		{config, {Name: "index.js", MIME: "application/javascript+module", Data: []byte("export default {}")}},
		{config, config},
		{{Name: config.Name, MIME: config.MIME, Data: []byte(`{"html_handling":"none","not_found_handling":"none","run_worker_first":true}`)}},
	} {
		if _, _, err := moduleBody("completion", modules, marker); err == nil {
			t.Fatal("unsafe assets-only upload accepted")
		}
	}
	if _, _, err := moduleBody("", []Module{config}, marker); err == nil {
		t.Fatal("asset completion token not required")
	}
}

func TestAssetsOnlyInitializationUploadsHealthBeforeCreatingWorker(t *testing.T) {
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.URL.Path != "/accounts/"+account+"/workers/scripts/reports/assets-upload-session" {
				t.Error("initialization did not start with native health assets")
			}
			var body struct{ Manifest map[string]any }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Manifest) != 1 || body.Manifest["/.__lc_delivery/health"] == nil {
				t.Fatal("health asset missing", body)
			}
			io.WriteString(w, `{"success":true,"result":{"jwt":"complete","buckets":[]}}`)
			return
		}
		if calls != 2 || r.Method != "PUT" || r.URL.Path != "/accounts/"+account+"/workers/scripts/reports" {
			t.Fatal("unexpected initialization request", r.Method, r.URL.Path)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err = json.NewDecoder(part).Decode(&metadata); err != nil {
			t.Fatal(err)
		}
		if metadata["main_module"] != nil || metadata["assets"].(map[string]any)["jwt"] != "complete" {
			t.Fatal("initialization is not assets-only", metadata)
		}
		if _, err = reader.NextPart(); err != io.EOF {
			t.Fatal("initialization uploaded executable modules", err)
		}
		io.WriteString(w, `{"success":true,"result":{"id":"version"}}`)
	})
	_, err := c.Initialize(context.Background(), account, "reports", []Module{
		{Name: "delivery-health.json", MIME: "application/json", Data: []byte(`{"protocol":1}`)},
		{Name: "delivery-assets-only.json", MIME: "application/json", Data: []byte(`{"html_handling":"none","not_found_handling":"none"}`)},
	}, `{"p":1,"i":"installation","t":"target","r":""}`)
	if err != nil || calls != 2 {
		t.Fatal("initialization failed", err, calls)
	}
}
