package staticdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"strings"

	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
)

// A distinct client is constructed for every pinned credential revision. There
// is no global token or fallback to the CMS cache-purge service.
type CFClient interface {
	Deployments(context.Context, string, string) ([]cf.Deployment, error)
	Version(context.Context, string, string, string) (json.RawMessage, error)
	Settings(context.Context, string, string) (json.RawMessage, error)
	Initialize(context.Context, string, string, []cf.Module, string) (cf.Version, error)
	UploadAssets(context.Context, string, string, []cf.File) (cf.CompletionToken, error)
	UploadVersion(context.Context, string, string, cf.CompletionToken, []cf.Module, string) (cf.Version, error)
	Activate(context.Context, string, string, string) (cf.Deployment, error)
}
type CloudflareRemote struct {
	Client CFClient
	Marker string
}

var _ Remote = (*CloudflareRemote)(nil)

type OwnershipMarker struct {
	Protocol       int    `json:"p"`
	InstallationID string `json:"i"`
	TargetID       string `json:"t"`
	ReleaseID      string `json:"r"`
}

func (m OwnershipMarker) String() string { b, _ := json.Marshal(m); return string(b) }
func (c *CloudflareRemote) Inspect(ctx context.Context, t Target) (RemoteState, error) {
	deployments, err := c.Client.Deployments(ctx, t.AccountID, t.WorkerName)
	var ae *cf.APIError
	if errors.As(err, &ae) && ae.Status == 404 {
		return RemoteState{}, nil
	}
	if err != nil {
		return RemoteState{}, remoteError(err, CodeCFUnavailable)
	}
	state := RemoteState{Exists: true}
	if len(deployments) == 0 {
		return state, nil
	}
	d := deployments[0]
	state.DeploymentID = d.ID
	if len(d.Versions) != 1 || d.Versions[0].Percentage != 100 {
		return state, nil
	}
	state.SingleVersion = true
	state.VersionID = d.Versions[0].VersionID
	raw, err := c.Client.Version(ctx, t.AccountID, t.WorkerName, state.VersionID)
	if err != nil {
		return state, remoteError(err, CodeCFUnavailable)
	}
	var v struct {
		Annotations map[string]string `json:"annotations"`
		Resources   struct {
			Bindings []struct{ Type, Name, Text string } `json:"bindings"`
			Script   struct {
				ETag string `json:"etag"`
			} `json:"script"`
		} `json:"resources"`
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		return state, failure(CodeRemoteDrift, "unreadable remote version", nil)
	}
	var marker OwnershipMarker
	var markerJSON string
	for _, b := range v.Resources.Bindings {
		if b.Type == "plain_text" && b.Name == "STATIC_DELIVERY_IDENTITY" {
			markerJSON = b.Text
		}
	}
	if tag := v.Annotations["workers/tag"]; strings.HasPrefix(tag, "lc1:") && markerJSON == "" && v.Resources.Script.ETag == "" {
		parts := strings.Split(tag, ":")
		if len(parts) == 4 && parts[1] != "" && parts[2] != "" {
			markerJSON = (OwnershipMarker{Protocol: 1, InstallationID: parts[1], TargetID: parts[2], ReleaseID: parts[3]}).String()
			// An assets-only version has no script ETag. Pin its immutable version
			// identity instead, so out-of-band uploads still trigger drift checks.
			state.CodeHash = "assets-only:" + state.VersionID
		}
	}
	if json.Unmarshal([]byte(markerJSON), &marker) == nil && marker.Protocol == 1 {
		state.InstallationID = marker.InstallationID
		state.TargetID = marker.TargetID
		state.ReleaseID = marker.ReleaseID
	}
	if state.CodeHash == "" {
		state.CodeHash = v.Resources.Script.ETag
	}
	settings, err := c.Client.Settings(ctx, t.AccountID, t.WorkerName)
	if err != nil {
		return state, remoteError(err, CodeCFUnavailable)
	}
	var canonical any
	if json.Unmarshal(settings, &canonical) != nil {
		return state, failure(CodeRemoteDrift, "unreadable remote settings", nil)
	}
	b, _ := json.Marshal(canonical)
	h := sha256.Sum256(b)
	state.SettingsHash = hex.EncodeToString(h[:])
	return state, nil
}
func cfModules(modules []Module) []cf.Module {
	out := make([]cf.Module, len(modules))
	for i, m := range modules {
		out[i] = cf.Module{Name: m.Name, MIME: m.MIME, Data: m.Data}
	}
	return out
}
func (c *CloudflareRemote) Initialize(ctx context.Context, t Target, m []Module) (RemoteVersion, error) {
	v, e := c.Client.Initialize(ctx, t.AccountID, t.WorkerName, cfModules(m), c.Marker)
	return RemoteVersion{ID: v.ID, Marker: c.Marker}, remoteError(e, CodeVersionUploadFailed)
}
func (c *CloudflareRemote) UploadAssets(ctx context.Context, t Target, files []UploadFile) (CompletionToken, error) {
	fs := make([]cf.File, len(files))
	for i, f := range files {
		h := sha256.Sum256(f.Data)
		if f.SHA256 != "" && f.SHA256 != hex.EncodeToString(h[:]) {
			return "", failure(CodeArtifactUnavailable, "asset checksum changed", nil)
		}
		fs[i] = cf.File{Path: f.PublicPath, MIME: f.MIME, Data: f.Data}
	}
	token, e := c.Client.UploadAssets(ctx, t.AccountID, t.WorkerName, fs)
	return CompletionToken(token), remoteError(e, CodeUploadFailed)
}
func (c *CloudflareRemote) UploadVersion(ctx context.Context, t Target, token CompletionToken, m []Module) (RemoteVersion, error) {
	var identity struct {
		Protocol       int    `json:"protocol"`
		InstallationID string `json:"installation_id"`
		TargetID       string `json:"target_id"`
		ReleaseID      string `json:"release_id"`
	}
	found := false
	for _, module := range m {
		if module.Name == "routes.json" {
			if found || json.Unmarshal(module.Data, &identity) != nil {
				return RemoteVersion{}, failure(CodeInvalidRequest, "invalid frozen Worker routes", nil)
			}
			found = true
		}
	}
	if !found || identity.Protocol != 1 || identity.InstallationID != t.InstallationID || identity.TargetID != t.ID.Hex() {
		return RemoteVersion{}, failure(CodeRevisionConflict, "Worker route ownership does not match deployment target", nil)
	}
	releaseID, err := primitive.ObjectIDFromHex(identity.ReleaseID)
	if err != nil || releaseID.IsZero() {
		return RemoteVersion{}, failure(CodeInvalidRequest, "frozen release identity is required", nil)
	}
	marker := OwnershipMarker{Protocol: 1, InstallationID: t.InstallationID, TargetID: t.ID.Hex(), ReleaseID: releaseID.Hex()}.String()
	v, e := c.Client.UploadVersion(ctx, t.AccountID, t.WorkerName, cf.CompletionToken(token), cfModules(m), marker)
	return RemoteVersion{ID: v.ID, Marker: marker}, remoteError(e, CodeVersionUploadFailed)
}
func (c *CloudflareRemote) Activate(ctx context.Context, t Target, v string) (RemoteDeployment, error) {
	d, e := c.Client.Activate(ctx, t.AccountID, t.WorkerName, v)
	return RemoteDeployment{ID: d.ID, VersionID: v}, remoteError(e, CodeCFActivationUnknown)
}
func remoteError(err error, fallback string) error {
	if err == nil {
		return nil
	}
	var ae *cf.APIError
	if errors.As(err, &ae) {
		if ae.Status == 401 || ae.Status == 403 {
			return failure(CodeCFPermission, "Cloudflare credential lacks access", err)
		}
		if ae.Status == 429 {
			return &Error{Code: CodeRateLimited, Message: "Cloudflare rate limit reached", Retryable: true, Cause: err}
		}
		if ae.Uncertain && fallback == CodeCFActivationUnknown {
			return failure(CodeCFActivationUnknown, "activation result must be reconciled", err)
		}
	}
	return failure(fallback, "Cloudflare operation did not complete", err)
}
