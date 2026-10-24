package cloudflare

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const account = "0123456789abcdef0123456789abcdef"

func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return newTestClient("api-secret", s.URL, s.Client(), func(context.Context, time.Duration) error { return nil })
}

func TestAssetHashProtocol(t *testing.T) {
	if got := AssetHash([]byte("asset fixture"), "png"); got != "e69ec1b31566d744f5ad1b5a6326ee2c" {
		t.Fatalf("wrong protocol hash: %s", got)
	}
}

func TestStreamingBucketRetryReplaysIdenticalBody(t *testing.T) {
	data := []byte(strings.Repeat("streamed fixture", 4096))
	hash := AssetHash(data, "png")
	attempts := 0
	var first []byte
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assets-upload-session") {
			io.WriteString(w, `{"success":true,"result":{"jwt":"upload","buckets":[["`+hash+`"]]}}`)
			return
		}
		attempts++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if attempts == 1 {
			first = body
			w.WriteHeader(503)
			return
		}
		if string(body) != string(first) {
			t.Error("retry changed streaming multipart bytes")
		}
		if r.ContentLength != -1 {
			t.Error("bucket was buffered")
		}
		io.WriteString(w, `{"success":true,"result":{"jwt":"complete"}}`)
	})
	if token, err := c.UploadAssets(context.Background(), account, "reports", []File{{Path: "/a.png", MIME: "image/png", Data: data}}); err != nil || token != "complete" || attempts != 2 {
		t.Fatalf("token=%s err=%v attempts=%d", token, err, attempts)
	}
}

func TestAssetUploadUsesFullManifestAndJWT(t *testing.T) {
	data := []byte("asset fixture")
	hash := AssetHash(data, "png")
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.Method != "POST" || r.URL.Path != "/accounts/"+account+"/workers/scripts/reports/assets-upload-session" || r.Header.Get("Authorization") != "Bearer api-secret" {
				t.Errorf("wrong session request %s %s", r.Method, r.URL.Path)
			}
			var body struct {
				Manifest map[string]struct {
					Hash string
					Size int
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Manifest) != 2 || body.Manifest["/a.png"].Hash != hash || body.Manifest["/a.png"].Size != len(data) {
				t.Errorf("incomplete manifest: %+v", body)
			}
			io.WriteString(w, `{"success":true,"result":{"jwt":"upload-secret","buckets":[["`+hash+`"]]}}`)
			return
		}
		if r.URL.Path != "/accounts/"+account+"/workers/assets/upload" || r.URL.Query().Get("base64") != "true" || r.Header.Get("Authorization") != "Bearer upload-secret" {
			t.Errorf("wrong bucket authorization or URL")
		}
		mr, err := r.MultipartReader()
		if r.ContentLength != -1 {
			t.Error("asset bucket should stream without a buffered body")
		}
		if err != nil {
			t.Fatal(err)
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(p)
		decoded, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil || string(decoded) != string(data) || p.FormName() != hash || p.Header.Get("Content-Type") != "image/png" {
			t.Errorf("bad asset part %s %s", p.FormName(), raw)
		}
		if _, err = mr.NextPart(); err != io.EOF {
			t.Errorf("unexpected extra part: %v", err)
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"success":true,"result":{"jwt":"completion-secret"}}`)
	})
	token, err := c.UploadAssets(context.Background(), account, "reports", []File{{Path: "/a.png", MIME: "image/png", Data: data}, {Path: "/b.html", MIME: "text/html", Data: []byte("ok")}})
	if err != nil || token != "completion-secret" || calls != 2 {
		t.Fatalf("%q %v calls=%d", token, err, calls)
	}
}

func TestEmptyBucketsNeedNoUpload(t *testing.T) {
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"success":true,"result":{"jwt":"done","buckets":[]}}`)
	})
	token, err := c.UploadAssets(context.Background(), account, "reports", nil)
	if err != nil || token != "done" || calls != 1 {
		t.Fatalf("%q %v calls=%d", token, err, calls)
	}
}

func TestVersionUploadDoesNotActivate(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/accounts/"+account+"/workers/scripts/reports/versions" {
			t.Errorf("must only create inactive version: %s %s", r.Method, r.URL.Path)
		}
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/form-data" {
			t.Fatal(mt)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		parts := map[string][]byte{}
		for {
			p, e := mr.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			parts[p.FormName()], _ = io.ReadAll(p)
		}
		var metadata struct {
			Main   string `json:"main_module"`
			Assets struct {
				JWT    string         `json:"jwt"`
				Config map[string]any `json:"config"`
			}
			Bindings    []struct{ Type, Name string }
			Annotations map[string]string
		}
		if err := json.Unmarshal(parts["metadata"], &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Main != "index.js" || metadata.Assets.JWT != "done" || metadata.Assets.Config["run_worker_first"] != true || metadata.Assets.Config["html_handling"] != "none" || metadata.Assets.Config["not_found_handling"] != "none" || len(metadata.Bindings) != 2 || metadata.Bindings[1].Name != "ASSETS" || metadata.Bindings[0].Name != "STATIC_DELIVERY_IDENTITY" || string(parts["index.js"]) != "export default {}" {
			t.Fatalf("incorrect metadata/parts: %+v", metadata)
		}
		io.WriteString(w, `{"success":true,"result":{"id":"version-1"}}`)
	})
	v, err := c.UploadVersion(context.Background(), account, "reports", "done", []Module{{Name: "index.js", MIME: "application/javascript+module", Data: []byte("export default {}")}}, "operation-1")
	if err != nil || v.ID != "version-1" {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestActivationNotRetriedAfterAmbiguousFailure(t *testing.T) {
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/accounts/"+account+"/workers/scripts/reports/deployments" {
			t.Error(r.URL.Path)
		}
		var b struct {
			Strategy string
			Versions []struct {
				ID         string `json:"version_id"`
				Percentage int
			}
		}
		json.NewDecoder(r.Body).Decode(&b)
		if b.Strategy != "percentage" || len(b.Versions) != 1 || b.Versions[0].Percentage != 100 || b.Versions[0].ID != "v1" {
			t.Errorf("bad activation %+v", b)
		}
		w.WriteHeader(503)
		io.WriteString(w, "api-secret")
	})
	_, err := c.Activate(context.Background(), account, "reports", "v1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Uncertain || calls != 1 || strings.Contains(err.Error(), "api-secret") {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestReadRateLimitBoundAndEnvelope(t *testing.T) {
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		io.WriteString(w, `{"success":false,"errors":[{"message":"api-secret"}]}`)
	})
	_, err := c.Subdomain(context.Background(), account)
	if err == nil || calls != 5 || strings.Contains(err.Error(), "api-secret") {
		t.Fatalf("%v calls=%d", err, calls)
	}
	c = clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false,"result":{"subdomain":"wrong"}}`)
	})
	if _, err = c.Subdomain(context.Background(), account); err == nil {
		t.Fatal("HTTP200 success=false accepted")
	}
}

func TestUploadExpiredJWTDoesNotRestartSession(t *testing.T) {
	calls := 0
	hash := AssetHash([]byte("x"), "png")
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			io.WriteString(w, `{"success":true,"result":{"jwt":"expired-secret","buckets":[["`+hash+`"]]}}`)
			return
		}
		w.WriteHeader(401)
		io.WriteString(w, `{"success":false}`)
	})
	_, err := c.UploadAssets(context.Background(), account, "reports", []File{{Path: "/x.png", Data: []byte("x"), MIME: "image/png"}})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || calls != 2 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}
