package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ssrfBlockedCIDRs are IP ranges that must never be contacted via user-supplied URLs.
var ssrfBlockedCIDRs = func() []*net.IPNet {
	var blocks []*net.IPNet
	for _, cidr := range []string{
		"0.0.0.0/8",      // "this" network
		"10.0.0.0/8",     // RFC1918 private
		"100.64.0.0/10",  // CGNAT shared address space
		"127.0.0.0/8",    // IPv4 loopback
		"169.254.0.0/16", // link-local / AWS EC2 metadata
		"172.16.0.0/12",  // RFC1918 private
		"192.168.0.0/16", // RFC1918 private
		"198.18.0.0/15",  // benchmarking
		"240.0.0.0/4",    // reserved
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA (includes fd00::/8)
		"fe80::/10",      // IPv6 link-local
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			blocks = append(blocks, block)
		}
	}
	return blocks
}()

// isPrivateOrReservedIP returns true if ip falls in any SSRF-blocked range.
func isPrivateOrReservedIP(ip net.IP) bool {
	for _, block := range ssrfBlockedCIDRs {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// maxRemoteAssetSize is the fixed MVP bound for POST /api/v1/assets/from-url
// (spec §25.3): maxSize = 50 MiB. Bodies are read up to maxSize+1 bytes; any
// length above maxSize is rejected with ASSET_TOO_LARGE and never saved, so a
// truncated prefix is never persisted.
const maxRemoteAssetSize int64 = 50 << 20

// validateRemoteAssetURL parses rawURL and enforces fetch safety: http/https
// only, a host, and no userinfo (spec §25.4 "URL userinfo"). DNS and IP
// restrictions are enforced at dial time by ssrfSafeClient.
func validateRemoteAssetURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("url must use http or https scheme")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url must have a host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("url must not contain userinfo")
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("invalid url")
	}
	return u, nil
}

// readBoundedRemoteBody enforces the 50 MiB bound (spec §25.3). A declared
// Content-Length above the bound is rejected before reading; unknown or
// chunked bodies are read up to maxSize+1 bytes and rejected when they exceed
// the bound. Oversize always returns an ASSET_TOO_LARGE error and never
// returns truncated bytes for saving.
func readBoundedRemoteBody(body io.Reader, contentLength int64) ([]byte, error) {
	if contentLength > maxRemoteAssetSize {
		return nil, fmt.Errorf("ASSET_TOO_LARGE: remote asset exceeds 50 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(body, maxRemoteAssetSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxRemoteAssetSize {
		return nil, fmt.Errorf("ASSET_TOO_LARGE: remote asset exceeds 50 MiB limit")
	}
	return data, nil
}

// ssrfSafeClient is an http.Client whose dialer rejects private/reserved IP ranges.
// It resolves the destination hostname at dial time and checks every returned IP,
// preventing SSRF and DNS-rebinding attacks.
var ssrfSafeClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL == nil {
			return fmt.Errorf("invalid redirect")
		}
		scheme := strings.ToLower(req.URL.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("redirect must use http or https")
		}
		if req.URL.User != nil {
			return fmt.Errorf("redirect must not contain userinfo")
		}
		if req.URL.Host == "" {
			return fmt.Errorf("redirect missing host")
		}
		return nil
	},
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address")
			}
			ips, err := net.DefaultResolver.LookupHost(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("could not resolve host")
			}
			for _, rawIP := range ips {
				ip := net.ParseIP(rawIP)
				if ip == nil || isPrivateOrReservedIP(ip) {
					return nil, fmt.Errorf("URL resolves to a private or restricted address")
				}
			}
			// Connect only to the first resolved public IP
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
		},
	},
}

// validAssetServePrefixes are the only path prefixes allowed for asset serve_path.
// This prevents callers from writing assets to arbitrary locations (e.g. /static/css/).
var validAssetServePrefixes = []string{"/assets/", "/images/", "/docs/", "/media/", "/files/"}

// isValidAssetServePath returns true when path begins with an allowed prefix.
func isValidAssetServePath(path string) bool {
	for _, prefix := range validAssetServePrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// API Asset endpoints

func (a *APIHandler) APIListAssets(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetView) {
		return
	}
	folder := r.URL.Query().Get("folder")

	assets, err := a.assetService.ListAssets(r.Context(), folder)
	if err != nil {
		a.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Strip binary data from response
	type AssetSummary struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		Folder      string `json:"folder"`
		FullPath    string `json:"full_path"`
		ServePath   string `json:"serve_path"`
		MimeType    string `json:"mime_type"`
		Size        int64  `json:"size"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
	}

	result := make([]AssetSummary, 0, len(assets))
	for _, asset := range assets {
		result = append(result, AssetSummary{
			ID:          asset.ID.Hex(),
			Filename:    asset.Filename,
			Folder:      asset.Folder,
			FullPath:    asset.FullPath,
			ServePath:   asset.ServePath,
			MimeType:    asset.MimeType,
			Size:        asset.Size,
			Description: asset.Description,
			CreatedAt:   asset.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	a.jsonResponse(w, http.StatusOK, result)
}

func (a *APIHandler) APIGetAsset(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetView) {
		return
	}
	id, err := primitive.ObjectIDFromHex(mux.Vars(r)["id"])
	if err != nil {
		a.jsonError(w, http.StatusBadRequest, "invalid asset ID")
		return
	}

	asset, err := a.assetService.GetAsset(r.Context(), id)
	if err != nil || asset == nil {
		a.jsonError(w, http.StatusNotFound, "asset not found")
		return
	}

	a.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id":          asset.ID.Hex(),
		"filename":    asset.Filename,
		"folder":      asset.Folder,
		"full_path":   asset.FullPath,
		"serve_path":  asset.ServePath,
		"mime_type":   asset.MimeType,
		"size":        asset.Size,
		"description": asset.Description,
		"created_at":  asset.CreatedAt,
	})
}

func (a *APIHandler) APIGetAssetByPath(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetView) {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		a.jsonError(w, http.StatusBadRequest, "path parameter is required")
		return
	}

	asset, err := a.assetService.GetAssetByPath(r.Context(), path)
	if err != nil || asset == nil {
		a.jsonError(w, http.StatusNotFound, "asset not found")
		return
	}

	a.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id":          asset.ID.Hex(),
		"filename":    asset.Filename,
		"folder":      asset.Folder,
		"full_path":   asset.FullPath,
		"serve_path":  asset.ServePath,
		"mime_type":   asset.MimeType,
		"size":        asset.Size,
		"description": asset.Description,
		"created_at":  asset.CreatedAt,
	})
}

func (a *APIHandler) APIUploadAsset(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetUpload) {
		return
	}

	var req struct {
		Filename    string `json:"filename"`
		ServePath   string `json:"serve_path"`
		DataBase64  string `json:"data_base64"`
		FilePath    string `json:"file_path"`
		Description string `json:"description"`
	}
	if err := a.decodeJSON(r, &req); err != nil {
		a.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Filename == "" || req.ServePath == "" {
		a.jsonError(w, http.StatusBadRequest, "filename and serve_path are required")
		return
	}
	if req.DataBase64 == "" && req.FilePath == "" {
		a.jsonError(w, http.StatusBadRequest, "either data_base64 or file_path is required")
		return
	}
	if !isValidAssetServePath(req.ServePath) {
		a.jsonError(w, http.StatusBadRequest, "serve_path must begin with /assets/, /images/, /docs/, /media/, or /files/")
		return
	}

	var data []byte
	if req.FilePath != "" {
		// Read file directly from local filesystem (avoids base64 size limits in MCP transport)
		var err error
		data, err = os.ReadFile(req.FilePath)
		if err != nil {
			a.jsonError(w, http.StatusBadRequest, fmt.Sprintf("failed to read file_path: %v", err))
			return
		}
	} else {
		var err error
		data, err = base64.StdEncoding.DecodeString(req.DataBase64)
		if err != nil {
			a.jsonError(w, http.StatusBadRequest, "invalid base64 data")
			return
		}
	}

	asset, err := a.assetService.UploadAsset(r.Context(), data, req.Filename, req.ServePath, req.Description)
	if err != nil {
		if errors.Is(err, services.ErrAssetCanonicalCollision) {
			a.jsonError(w, http.StatusConflict, err.Error())
			return
		}
		a.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.auditLog(r, "asset.upload", "asset", asset.ID.Hex(), map[string]interface{}{"filename": asset.Filename, "serve_path": asset.ServePath})
	a.jsonResponse(w, http.StatusCreated, map[string]interface{}{
		"id":         asset.ID.Hex(),
		"filename":   asset.Filename,
		"serve_path": asset.ServePath,
		"mime_type":  asset.MimeType,
		"size":       asset.Size,
	})
}

func (a *APIHandler) APIDeleteAsset(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetDelete) {
		return
	}

	id, err := primitive.ObjectIDFromHex(mux.Vars(r)["id"])
	if err != nil {
		a.jsonError(w, http.StatusBadRequest, "invalid asset ID")
		return
	}

	if err := a.assetService.DeleteAsset(r.Context(), id); err != nil {
		a.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.auditLog(r, "asset.delete", "asset", id.Hex(), nil)
	a.jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (a *APIHandler) APIListAssetFolders(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetView) {
		return
	}
	folders, err := a.assetService.ListFolders(r.Context())
	if err != nil {
		a.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.jsonResponse(w, http.StatusOK, folders)
}

// APIUploadAssetFromURL fetches a remote URL and stores it as an asset.
// Body: {"url": "https://...", "serve_path": "/assets/foo.png", "description": "..."}
func (a *APIHandler) APIUploadAssetFromURL(w http.ResponseWriter, r *http.Request) {
	if !a.requirePermission(w, r, auth.PermAssetUpload) {
		return
	}

	var req struct {
		URL         string `json:"url"`
		ServePath   string `json:"serve_path"`
		Description string `json:"description"`
	}
	if err := a.decodeJSON(r, &req); err != nil {
		a.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		a.jsonError(w, http.StatusBadRequest, "url is required")
		return
	}

	// Strict URL validation before any DNS resolution: http/https only, a
	// host, and no userinfo (spec §25.4).
	parsedURL, err := validateRemoteAssetURL(req.URL)
	if err != nil {
		a.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Fail fast on an unsafe serve_path before fetching remote bytes.
	if req.ServePath != "" && !isValidAssetServePath(req.ServePath) {
		a.jsonError(w, http.StatusBadRequest, "serve_path must begin with /assets/, /images/, /docs/, /media/, or /files/")
		return
	}

	// ssrfSafeClient resolves the host and blocks private/reserved IPs before connecting
	resp, err := ssrfSafeClient.Get(parsedURL.String())
	if err != nil {
		// Return a generic error — never echo network internals back to the caller
		a.jsonError(w, http.StatusBadGateway, "failed to fetch remote URL")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		a.jsonError(w, http.StatusBadGateway, fmt.Sprintf("remote server returned %d", resp.StatusCode))
		return
	}

	// Bounded read: Content-Length pre-check plus maxSize+1 LimitReader with an
	// explicit length check. Oversize never saves truncated bytes.
	data, err := readBoundedRemoteBody(resp.Body, resp.ContentLength)
	if err != nil {
		if strings.Contains(err.Error(), "ASSET_TOO_LARGE") {
			a.jsonError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		a.jsonError(w, http.StatusBadGateway, "failed to read remote response")
		return
	}

	// Derive filename from URL path if serve_path not provided
	servePath := req.ServePath
	if servePath == "" {
		filename := filepath.Base(parsedURL.Path)
		if filename == "" || filename == "." || filename == "/" {
			filename = "asset"
		}
		servePath = "/assets/" + filename
	}
	if !isValidAssetServePath(servePath) {
		a.jsonError(w, http.StatusBadRequest, "serve_path must begin with /assets/, /images/, /docs/, /media/, or /files/")
		return
	}
	filename := filepath.Base(servePath)

	asset, err := a.assetService.UploadAsset(r.Context(), data, filename, servePath, req.Description)
	if err != nil {
		if errors.Is(err, services.ErrAssetCanonicalCollision) {
			a.jsonError(w, http.StatusConflict, err.Error())
			return
		}
		a.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.auditLog(r, "asset.upload", "asset", asset.ID.Hex(), map[string]interface{}{
		"filename": asset.Filename, "serve_path": asset.ServePath, "source_url": req.URL,
	})
	a.jsonResponse(w, http.StatusCreated, map[string]interface{}{
		"id":         asset.ID.Hex(),
		"filename":   asset.Filename,
		"serve_path": asset.ServePath,
		"mime_type":  asset.MimeType,
		"size":       asset.Size,
	})
}
