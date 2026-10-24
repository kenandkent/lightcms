package staticdelivery

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ReleaseVerifier uses the same HTTPS/DNS policy as origin initialization.
// Each round verifies the entire retained manifest, not only the API response.
type ReleaseVerifier struct {
	client *http.Client
	wait   func(context.Context, time.Duration) error
}

func NewReleaseVerifier() *ReleaseVerifier {
	return &ReleaseVerifier{client: NewOriginVerifier().client, wait: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
}
func (v *ReleaseVerifier) Check(ctx context.Context, r ReleaseManifest) error {
	if e := validateRelease(r); e != nil {
		return e
	}
	target := Target{Record: Record{ID: r.TargetID}, Origin: r.Origin, InstallationID: r.InstallationID}
	state := RemoteState{InstallationID: r.InstallationID, TargetID: r.TargetID.Hex(), ReleaseID: r.ID.Hex()}
	paths := map[string]string{}
	for canonical, p := range r.Pages {
		paths[p.File] = canonical
	}
	names := make([]string, 0, len(r.Files))
	for name := range r.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for round := 0; round < 3; round++ {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := (&OriginVerifier{client: v.client}).Check(ctx, target, state); e != nil {
			return failure(CodeVerificationFailed, "public Worker identity has not converged to the candidate release", nil)
		}
		for _, name := range names {
			f := r.Files[name]
			publicPath := name
			if full, ok := paths[name]; ok {
				publicPath = full
			}
			if e := v.checkFile(ctx, r, publicPath, f, false); e != nil {
				return e
			}
			if _, page := paths[name]; page {
				if e := v.checkFile(ctx, r, publicPath, f, true); e != nil {
					return e
				}
			}
		}
		for _, path := range removedURLs(r) {
			for _, head := range []bool{false, true} {
				if err := v.checkAbsent(ctx, r, path, head); err != nil {
					return err
				}
			}
		}
		if round < 2 {
			if e := v.wait(ctx, 2*time.Second); e != nil {
				return e
			}
		}
	}
	return nil
}

func removedURLs(r ReleaseManifest) []string {
	paths := map[string]bool{}
	for _, canonical := range r.RemovedPaths {
		paths[canonical] = true
		if canonical != "/" {
			for _, alias := range []string{canonical + "/", canonical + ".html"} {
				if _, exists := r.Pages[alias]; !exists {
					paths[alias] = true
				}
			}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}
func (v *ReleaseVerifier) checkAbsent(ctx context.Context, r ReleaseManifest, path string, head bool) error {
	origin, err := NormalizeOrigin(r.Origin)
	if err != nil {
		return err
	}
	method := "GET"
	if head {
		method = "HEAD"
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+(&url.URL{Path: path}).EscapedPath(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	client := *v.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return failure(CodeVerificationFailed, "withdrawn page verification failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		return failure(CodeVerificationFailed, "withdrawn page or alias still resolves instead of returning 404", nil)
	}
	return nil
}
func (v *ReleaseVerifier) checkFile(ctx context.Context, r ReleaseManifest, publicPath string, f ReleaseFile, head bool) error {
	origin, e := NormalizeOrigin(r.Origin)
	if e != nil {
		return e
	}
	method := "GET"
	if head {
		method = "HEAD"
	}
	req, e := http.NewRequestWithContext(ctx, method, origin+(&url.URL{Path: publicPath}).EscapedPath(), nil)
	if e != nil {
		return failure(CodeVerificationFailed, "invalid release verification URL", nil)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, e := v.client.Do(req)
	if e != nil {
		return failure(CodeVerificationFailed, "public release request failed", nil)
	}
	defer resp.Body.Close()
	kind, _, mimeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	expected, _, expectedErr := mime.ParseMediaType(f.MIME)
	etag := resp.Header.Get("ETag")
	validETag := etag == `"`+f.SHA256+`"`
	if nativeDelivery(r) {
		// Cloudflare owns its validator. Integrity is checked below against the
		// actual response bytes; native ETag is not assumed to be SHA-256.
		validETag = len(etag) >= 2 && strings.HasSuffix(etag, `"`) && (strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/"`))
	}
	if resp.StatusCode != 200 || resp.Header.Get("X-LC-Release") != r.ID.Hex() || mimeErr != nil || expectedErr != nil || kind != expected || !validETag {
		return failure(CodeVerificationFailed, "public release status, MIME, ETag or release marker does not match", nil)
	}
	cache := "public,max-age=31536000,immutable"
	if kind == "text/html" {
		cache = "no-cache"
	}
	validCache := strings.ReplaceAll(resp.Header.Get("Cache-Control"), " ", "") == cache
	if nativeDelivery(r) && kind == "text/html" {
		validCache = revalidatesHTML(resp.Header.Get("Cache-Control"))
	}
	if !validCache {
		return failure(CodeVerificationFailed, "public release cache policy does not match", nil)
	}
	limit := f.Size + 1
	if head {
		limit = 1
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, limit))
	if e != nil {
		return failure(CodeVerificationFailed, "public release response body could not be read", nil)
	}
	if head {
		if len(body) != 0 {
			return failure(CodeVerificationFailed, "HEAD returned a response body", nil)
		}
	} else if int64(len(body)) != f.Size || releaseHash(body) != f.SHA256 {
		return failure(CodeVerificationFailed, "public release bytes do not match the immutable manifest", nil)
	}
	return nil
}
