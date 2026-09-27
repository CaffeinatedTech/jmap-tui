package jmapclient

import (
	"net/url"
	"strings"
)

// originKey normalizes raw to its origin — scheme, host, and effective
// port (default ports omitted) — lowercased, so two keys are equal if and
// only if the URLs are same-origin (SECURITY_AUDIT_PLAN.md D-2). ok is
// false when raw is not an absolute URL with a host; callers treat that as
// "no origin, therefore not trusted" (fail closed for credentials).
func originKey(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return originKeyURL(u), true
}

// originKeyURL is originKey for an already-parsed URL. Host is
// lowercased, userinfo never participates (it is not part of an origin),
// and :80/:443 are folded into the scheme so http://host and
// http://host:80 compare equal.
func originKeyURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" ||
		(scheme == "http" && port == "80") ||
		(scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}
