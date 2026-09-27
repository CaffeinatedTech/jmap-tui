package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateServerURL enforces the URL policy of SECURITY_AUDIT_PLAN.md
// D-1/D-2 on every server URL the app will send credentials to (findings
// F-5, F-6): absolute http(s) URL, no userinfo, and cleartext http://
// only for loopback hosts — mockjmap and local Stalwart experiments keep
// working while a remote server can never receive Basic auth in the
// clear. It runs at config validation and on the --url/smoke flag paths;
// the wizard calls it through normalizeServerURL.
//
// Error messages never echo the raw URL: a URL may embed user:pass@ and
// these errors are printed to the terminal (FR-K2).
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("not a valid server URL; e.g. https://mail.example.com")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("cleartext http:// is only allowed for loopback hosts (127.0.0.1, ::1, localhost), not host %q; use https://", u.Hostname())
		}
	default:
		return fmt.Errorf("server URL scheme must be https (or http for loopback), got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("server URL must not embed credentials (user:pass@host); set the username in config instead")
	}
	return nil
}

// isLoopbackHost reports whether host (already without port) is a
// loopback address: localhost (any case) or an IP in 127.0.0.0/8 / ::1.
// Literal checks only — DNS resolution has no place in config
// validation, so hostnames that merely resolve to loopback are not
// carve-ed out.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
