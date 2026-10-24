package cloudflare

import (
	"context"
	"errors"
)

type ProbeResult struct {
	Readable, ListReadable bool
	Subdomain              string
}

func (c *Client) Probe(ctx context.Context, account, worker string) (ProbeResult, error) {
	p, e := accountPath(account)
	if e != nil {
		return ProbeResult{}, e
	}
	var workers []struct {
		ID string `json:"id"`
	}
	e = c.request(ctx, "GET", p+"/scripts", c.token, "application/json", nil, &workers, true, false)
	result := ProbeResult{ListReadable: e == nil, Readable: e == nil}
	if e != nil {
		var ae *APIError
		if !errors.As(e, &ae) || ae.Status != 403 || worker == "" {
			return result, e
		}
		if _, e = c.Settings(ctx, account, worker); e != nil {
			return result, e
		}
		result.Readable = true
	}
	domain, e := c.Subdomain(ctx, account)
	// Subdomain read may itself be scope-restricted. Keep the positive Worker
	// check: custom domains can work without workers.dev access.
	if e == nil {
		result.Subdomain = domain
	} else {
		var ae *APIError
		if !errors.As(e, &ae) || (ae.Status != 403 && ae.Status != 404) {
			return result, e
		}
	}
	return result, nil
}
