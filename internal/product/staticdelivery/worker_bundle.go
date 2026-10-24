package staticdelivery

import (
	"encoding/json"
	reportstatic "github.com/metavert-io/lightcms/v7/workers/report-static"
)

type workerFile struct {
	File   string `json:"file,omitempty"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime,omitempty"`
}

func ReleaseModules(r ReleaseManifest) ([]Module, error) {
	pages := map[string]workerFile{}
	assets := map[string]workerFile{}
	aliases := map[string]string{}
	for full, p := range r.Pages {
		pages[full] = workerFile{File: publicAssetPath(r, full, p.File), SHA256: p.SHA256}
	}
	for full, f := range r.Files {
		if len(full) >= 12 && full[:12] == "/_lc-assets/" {
			assets[full] = workerFile{SHA256: f.SHA256, MIME: f.MIME}
		}
	}
	for full := range r.Pages {
		if full == "/" {
			continue
		}
		for _, alias := range []string{full + "/", full + ".html"} {
			if _, exists := pages[alias]; exists {
				continue
			}
			if previous, exists := aliases[alias]; exists && previous != full {
				delete(aliases, alias)
				continue
			}
			aliases[alias] = full
		}
	}
	routes := struct {
		Protocol       int                   `json:"protocol"`
		InstallationID string                `json:"installation_id"`
		TargetID       string                `json:"target_id"`
		ReleaseID      string                `json:"release_id"`
		Environment    string                `json:"environment"`
		Pages          map[string]workerFile `json:"pages"`
		Assets         map[string]workerFile `json:"assets"`
		Aliases        map[string]string     `json:"aliases"`
	}{1, r.InstallationID, r.TargetID.Hex(), r.ID.Hex(), r.Environment, pages, assets, aliases}
	raw, e := json.Marshal(routes)
	if e != nil {
		return nil, e
	}
	if r.DeliveryMode == AssetsOnlyV1 {
		config, err := nativeAssetConfig(r)
		if err != nil {
			return nil, err
		}
		// routes.json is a private ownership/planning artifact. The upload client
		// consumes it without sending any module parts to Cloudflare.
		return []Module{{Name: "routes.json", MIME: "application/json", Data: raw}, {Name: "delivery-assets-only.json", MIME: "application/json", Data: config}}, nil
	}
	embedded, e := reportstatic.Modules(raw)
	if e != nil {
		return nil, failure(CodeAssetLimitExceeded, "Worker module size exceeds plan limit", e)
	}
	out := make([]Module, len(embedded))
	for i, m := range embedded {
		out[i] = Module{Name: m.Name, MIME: m.MIME, Data: m.Data}
	}
	if r.DeliveryMode == AssetsFirstV1 {
		config, e := nativeAssetConfig(r)
		if e != nil {
			return nil, e
		}
		total := len(config)
		for _, m := range out {
			total += len(m.Data)
		}
		if total > 3<<20 {
			return nil, failure(CodeAssetLimitExceeded, "Worker configuration exceeds module limit", nil)
		}
		out = append(out, Module{Name: "delivery-assets-config.json", MIME: "application/json", Data: config})
	}
	return out, nil
}
