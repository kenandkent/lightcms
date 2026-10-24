package cloudflare

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// Version annotations survive assets-only uploads; runtime bindings do not.
// A compact tag fits Cloudflare's 128-character version tag limit even with
// three ObjectIDs. Never put credentials in this public deployment identity.
func assetsOnlyBody(token CompletionToken, modules []Module, marker string, configJSON []byte) ([]byte, string, error) {
	var identity struct {
		Protocol     int    `json:"p"`
		Installation string `json:"i"`
		Target       string `json:"t"`
		Release      string `json:"r"`
	}
	if token == "" || json.Unmarshal([]byte(marker), &identity) != nil || identity.Protocol != 1 || identity.Installation == "" || identity.Target == "" {
		return nil, "", errors.New("assets-only upload requires completion token and ownership identity")
	}
	for _, id := range []string{identity.Installation, identity.Target, identity.Release} {
		if strings.ContainsAny(id, ":\r\n\x00") {
			return nil, "", errors.New("invalid assets-only ownership identity")
		}
	}
	tag := "lc1:" + identity.Installation + ":" + identity.Target + ":" + identity.Release
	if len(tag) > 128 {
		return nil, "", errors.New("assets-only ownership tag exceeds limit")
	}
	var config map[string]any
	if json.Unmarshal(configJSON, &config) != nil || config["html_handling"] != "none" || config["not_found_handling"] != "none" || config["run_worker_first"] != nil {
		return nil, "", errors.New("invalid assets-only routing configuration")
	}
	seen := map[string]bool{}
	total := 0
	for _, m := range modules {
		total += len(m.Data)
		if seen[m.Name] || m.MIME != "application/json" || (m.Name != "routes.json" && m.Name != "delivery-assets-only.json") || total > 3<<20 {
			return nil, "", errors.New("assets-only upload cannot contain executable modules")
		}
		seen[m.Name] = true
	}
	metadata := map[string]any{"compatibility_date": "2026-10-19", "bindings": []any{}, "annotations": map[string]string{"workers/tag": tag}, "assets": map[string]any{"jwt": string(token), "config": config}}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	p, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if err = json.NewEncoder(p).Encode(metadata); err != nil {
		return nil, "", err
	}
	if err = w.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), w.FormDataContentType(), nil
}
