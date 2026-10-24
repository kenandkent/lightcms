package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path"
	"strings"
)

type Module struct {
	Name, MIME string
	Data       []byte
}
type Version struct {
	ID string `json:"id"`
}

func moduleBody(token CompletionToken, modules []Module, marker string) ([]byte, string, error) {
	if len(modules) == 0 {
		return nil, "", errors.New("Worker modules required")
	}
	for _, module := range modules {
		if module.Name == "delivery-assets-only.json" {
			return assetsOnlyBody(token, modules, marker, module.Data)
		}
	}
	metadata := map[string]any{"main_module": modules[0].Name, "compatibility_date": "2026-10-19", "annotations": map[string]string{"workers/message": marker}}
	bindings := []map[string]string{{"type": "plain_text", "name": "STATIC_DELIVERY_IDENTITY", "text": marker}}
	if token != "" {
		bindings = append(bindings, map[string]string{"type": "assets", "name": "ASSETS"})
		metadata["assets"] = map[string]any{"jwt": string(token), "config": map[string]any{"run_worker_first": true, "html_handling": "none", "not_found_handling": "none"}}
		for _, module := range modules {
			if module.Name != "delivery-assets-config.json" {
				continue
			}
			var config struct {
				RunWorkerFirst   []string `json:"run_worker_first"`
				HTMLHandling     string   `json:"html_handling"`
				NotFoundHandling string   `json:"not_found_handling"`
				Headers          string   `json:"_headers"`
				Redirects        string   `json:"_redirects"`
			}
			if e := json.Unmarshal(module.Data, &config); e != nil || len(config.RunWorkerFirst) != 1 || config.RunWorkerFirst[0] != "/.__lc_delivery/health" || config.HTMLHandling != "none" || config.NotFoundHandling != "none" {
				return nil, "", errors.New("invalid static delivery asset configuration")
			}
			metadata["assets"] = map[string]any{"jwt": string(token), "config": config}
		}
	}
	metadata["bindings"] = bindings
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="metadata"`)
	hdr.Set("Content-Type", "application/json")
	p, _ := mw.CreatePart(hdr)
	if err := json.NewEncoder(p).Encode(metadata); err != nil {
		return nil, "", err
	}
	seen := map[string]bool{"metadata": true}
	total := 0
	for _, m := range modules {
		total += len(m.Data)
		if m.Name == "" || path.Clean(m.Name) != m.Name || strings.HasPrefix(m.Name, "/") || strings.HasPrefix(m.Name, "..") || strings.ContainsAny(m.Name, "\\\x00\r\n") || seen[m.Name] || total > 3<<20 {
			return nil, "", errors.New("invalid Worker module")
		}
		seen[m.Name] = true
		// Internal frozen configuration is consumed into assets metadata above;
		// it is not an executable/importable Worker module.
		if m.Name == "delivery-assets-config.json" {
			if token == "" {
				return nil, "", errors.New("asset configuration requires completion token")
			}
			continue
		}
		if m.MIME != "application/javascript+module" && m.MIME != "text/plain" {
			return nil, "", errors.New("unsupported Worker module MIME")
		}
		hdr = textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": m.Name, "filename": m.Name}))
		hdr.Set("Content-Type", m.MIME)
		p, e := mw.CreatePart(hdr)
		if e != nil {
			return nil, "", e
		}
		if _, e = p.Write(m.Data); e != nil {
			return nil, "", e
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), mw.FormDataContentType(), nil
}

// Initialize is reserved for a durable initialization intent on an absent,
// dedicated Worker. Ordinary updates must use UploadVersion instead.
func (c *Client) Initialize(ctx context.Context, account, worker string, modules []Module, marker string) (Version, error) {
	for _, m := range modules {
		if m.Name != "delivery-health.json" {
			continue
		}
		token, err := c.UploadAssets(ctx, account, worker, []File{{Path: "/.__lc_delivery/health", MIME: "application/json", Data: m.Data}})
		if err != nil {
			return Version{}, err
		}
		remaining := make([]Module, 0, len(modules)-1)
		for _, part := range modules {
			if part.Name != "delivery-health.json" {
				remaining = append(remaining, part)
			}
		}
		return c.upload(ctx, account, worker, "PUT", token, remaining, marker)
	}
	return c.upload(ctx, account, worker, "PUT", "", modules, marker)
}
func (c *Client) UploadVersion(ctx context.Context, account, worker string, token CompletionToken, modules []Module, marker string) (Version, error) {
	if token == "" {
		return Version{}, errors.New("asset completion token required")
	}
	return c.upload(ctx, account, worker, "POST", token, modules, marker)
}
func (c *Client) upload(ctx context.Context, account, worker, method string, token CompletionToken, modules []Module, marker string) (Version, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return Version{}, e
	}
	body, ct, e := moduleBody(token, modules, marker)
	if e != nil {
		return Version{}, e
	}
	if method == "POST" {
		p += "/versions"
	}
	var v Version
	e = c.request(ctx, method, p, c.token, ct, body, &v, false, true)
	if e == nil && v.ID == "" {
		e = &APIError{Uncertain: true}
	}
	return v, e
}
func (c *Client) Activate(ctx context.Context, account, worker, version string) (Deployment, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return Deployment{}, e
	}
	if version == "" {
		return Deployment{}, errors.New("version ID required")
	}
	body, _ := json.Marshal(map[string]any{"strategy": "percentage", "versions": []map[string]any{{"version_id": version, "percentage": 100}}})
	var d Deployment
	e = c.request(ctx, "POST", p+"/deployments", c.token, "application/json", body, &d, false, true)
	if e == nil && d.ID == "" {
		e = &APIError{Uncertain: true}
	}
	return d, e
}
