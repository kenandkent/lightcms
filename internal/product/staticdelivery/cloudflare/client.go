// Package cloudflare implements the static-delivery REST protocol independently
// of the existing CMS cache-purge client. A client belongs to one credential revision.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Options struct{ Timeout time.Duration }
type Client struct {
	token, base string
	http        *http.Client
	wait        func(context.Context, time.Duration) error
}
type APIError struct {
	Status                    int
	Retryable, Uncertain      bool
	Method, Path, Ray, Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Cloudflare request failed (HTTP %d; %s %s; ray=%s): %s", e.Status, e.Method, e.Path, e.Ray, e.Detail)
}

var diagnosticSecret = regexp.MustCompile(`[A-Za-z0-9_./+=-]{32,}`)

func (c *Client) diagnosticError(method, requestPath, token string, resp *http.Response, raw []byte, retryable, uncertain bool) *APIError {
	var envelope struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &envelope)
	details := []string{}
	for i, item := range envelope.Errors {
		if i == 5 {
			break
		}
		message := item.Message
		for _, secret := range []string{c.token, token} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		message = diagnosticSecret.ReplaceAllString(message, "[redacted]")
		message = strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, message)
		if len(message) > 512 {
			message = message[:512]
		}
		details = append(details, fmt.Sprintf("%d: %s", item.Code, message))
	}
	// Never retain raw response bodies, request bodies or authorization headers.
	return &APIError{Status: resp.StatusCode, Retryable: retryable, Uncertain: uncertain, Method: method, Path: strings.SplitN(requestPath, "?", 2)[0], Ray: resp.Header.Get("CF-Ray"), Detail: strings.Join(details, "; ")}
}

func NewClient(token string, opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{token: token, base: "https://api.cloudflare.com/client/v4", http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, wait: wait}
}
func newTestClient(token, base string, h *http.Client, w func(context.Context, time.Duration) error) *Client {
	clone := *h
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{token: token, base: base, http: &clone, wait: w}
}
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var accountPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
var workerPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func accountPath(account string) (string, error) {
	if !accountPattern.MatchString(account) {
		return "", errors.New("invalid Cloudflare account ID")
	}
	return "/accounts/" + account + "/workers", nil
}
func scriptPath(account, worker string) (string, error) {
	p, err := accountPath(account)
	if err != nil {
		return "", err
	}
	if !workerPattern.MatchString(worker) {
		return "", errors.New("invalid Worker name")
	}
	return p + "/scripts/" + worker, nil
}

// Only safe reads and repeatable asset bucket transfers retry automatically.
// Creating versions or activating deployments must be reconciled by the caller.
func (c *Client) request(ctx context.Context, method, path, token, contentType string, body []byte, out any, retry, uncertain bool) error {
	return c.requestBody(ctx, method, path, token, contentType, func() io.ReadCloser {
		if len(body) == 0 {
			return http.NoBody
		}
		return io.NopCloser(bytes.NewReader(body))
	}, int64(len(body)), out, retry, uncertain)
}

// A fresh reader per attempt makes bounded streaming uploads retryable without
// retaining a second, base64-expanded copy of the entire upload bucket.
func (c *Client) requestBody(ctx context.Context, method, path, token, contentType string, body func() io.ReadCloser, length int64, out any, retry, uncertain bool) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("Cloudflare credential unavailable")
	}
	attempts := 1
	if retry {
		attempts = 5
	}
	for n := 0; n < attempts; n++ {
		reader := body()
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
		if err != nil {
			reader.Close()
			return errors.New("invalid Cloudflare request")
		}
		req.ContentLength = length
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		reader.Close()
		if err != nil {
			if ctx.Err() != nil && !uncertain {
				return ctx.Err()
			}
			if retry && ctx.Err() == nil && n+1 < attempts {
				if e := c.wait(ctx, time.Second<<n); e != nil {
					return e
				}
				continue
			}
			detail := "transport error before an HTTP response"
			if errors.Is(err, context.DeadlineExceeded) {
				detail = "request deadline exceeded before an HTTP response"
			}
			return &APIError{Retryable: true, Uncertain: uncertain, Method: method, Path: strings.SplitN(path, "?", 2)[0], Detail: detail}
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if retry && n+1 < attempts {
				delay := time.Second << n
				if v, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && v >= 0 {
					delay = time.Duration(v) * time.Second
				} else if at, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
					delay = time.Until(at)
				}
				if delay < 0 {
					delay = 0
				}
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
				if err = c.wait(ctx, delay); err != nil {
					return err
				}
				continue
			}
			return c.diagnosticError(method, path, token, resp, raw, true, uncertain)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return c.diagnosticError(method, path, token, resp, raw, false, false)
		}
		var envelope struct {
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
		}
		if readErr != nil || len(raw) > 8<<20 || json.Unmarshal(raw, &envelope) != nil || !envelope.Success {
			return c.diagnosticError(method, path, token, resp, raw, false, uncertain)
		}
		if out != nil {
			if len(envelope.Result) == 0 || string(envelope.Result) == "null" || json.Unmarshal(envelope.Result, out) != nil {
				return c.diagnosticError(method, path, token, resp, raw, false, uncertain)
			}
		}
		return nil
	}
	return &APIError{Retryable: true, Uncertain: uncertain}
}
func (c *Client) Subdomain(ctx context.Context, account string) (string, error) {
	p, e := accountPath(account)
	if e != nil {
		return "", e
	}
	var r struct{ Subdomain string }
	e = c.request(ctx, "GET", p+"/subdomain", c.token, "application/json", nil, &r, true, false)
	return r.Subdomain, e
}
func (c *Client) EnableSubdomain(ctx context.Context, account, worker string) error {
	p, e := scriptPath(account, worker)
	if e != nil {
		return e
	}
	return c.request(ctx, "POST", p+"/subdomain", c.token, "application/json", []byte(`{"enabled":true,"previews_enabled":false}`), nil, false, true)
}

type Deployment struct {
	ID       string `json:"id"`
	Versions []struct {
		VersionID  string  `json:"version_id"`
		Percentage float64 `json:"percentage"`
	} `json:"versions"`
}

func (c *Client) Deployments(ctx context.Context, account, worker string) ([]Deployment, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return nil, e
	}
	var r struct {
		Deployments []Deployment `json:"deployments"`
	}
	e = c.request(ctx, "GET", p+"/deployments", c.token, "application/json", nil, &r, true, false)
	return r.Deployments, e
}
func (c *Client) Settings(ctx context.Context, account, worker string) (json.RawMessage, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return nil, e
	}
	var r json.RawMessage
	e = c.request(ctx, "GET", p+"/settings", c.token, "application/json", nil, &r, true, false)
	return r, e
}
func (c *Client) Version(ctx context.Context, account, worker, id string) (json.RawMessage, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return nil, e
	}
	if id == "" {
		return nil, errors.New("version ID required")
	}
	var r json.RawMessage
	e = c.request(ctx, "GET", p+"/versions/"+url.PathEscape(id), c.token, "application/json", nil, &r, true, false)
	return r, e
}
