package staticdelivery

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type OriginVerifier struct{ client *http.Client }

func publicAddress(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		_, network, _ := net.ParseCIDR(raw)
		if network.Contains(ip) {
			return false
		}
	}
	return true
}
func NewOriginVerifier() *OriginVerifier {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil || port != "443" {
			return nil, failure(CodeOriginNotReady, "HTTPS origin required", nil)
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil || len(ips) == 0 {
			return nil, failure(CodeOriginNotReady, "public origin DNS lookup failed", nil)
		}
		for _, ip := range ips {
			if !publicAddress(ip.IP) {
				return nil, failure(CodeOriginNotReady, "origin resolves to a non-public address", nil)
			}
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, failure(CodeOriginNotReady, "public origin connection failed", nil)
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	return &OriginVerifier{client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (v *OriginVerifier) Check(ctx context.Context, t Target, state RemoteState) error {
	origin, e := NormalizeOrigin(t.Origin)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", origin+"/.__lc_delivery/health", nil)
	if e != nil {
		return failure(CodeOriginNotReady, "invalid public origin", nil)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, e := v.client.Do(req)
	if e != nil {
		return failure(CodeOriginNotReady, "public origin is not reachable over HTTPS", nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return failure(CodeOriginNotReady, "origin does not serve the expected Worker health response", nil)
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if e != nil || len(raw) > 4096 {
		return failure(CodeOriginNotReady, "invalid Worker health response", nil)
	}
	var health struct {
		Protocol       int    `json:"protocol"`
		InstallationID string `json:"installation_id"`
		TargetID       string `json:"target_id"`
		ReleaseID      string `json:"release_id"`
	}
	if json.Unmarshal(raw, &health) != nil || health.Protocol != 1 || health.InstallationID != t.InstallationID || health.TargetID != t.ID.Hex() || health.InstallationID != state.InstallationID || health.TargetID != state.TargetID || health.ReleaseID != state.ReleaseID {
		return failure(CodeOriginNotReady, "origin ownership or release marker does not match the selected Worker", nil)
	}
	return nil
}
