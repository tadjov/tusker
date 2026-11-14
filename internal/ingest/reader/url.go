package reader

import (
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// NormalizeURL trims and validates raw, returning a cleaned absolute http(s)
// URL without its fragment and utm parameters. The bool is false when raw is
// empty or not a valid http(s) URL.
func NormalizeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	u.RawQuery = withoutTrackingParams(u.RawQuery)
	return u.String(), true
}

// URLIdentity names the page a normalized URL points to, so its http, https,
// www and trailing-slash spellings yield one article id.
func URLIdentity(normalizedURL string) string {
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return normalizedURL
	}
	identity := strings.TrimPrefix(u.Host, "www.") + strings.TrimSuffix(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		identity += "?" + u.RawQuery
	}
	return identity
}

// withoutTrackingParams edits the raw query, since url.Values would re-sort and re-encode it.
func withoutTrackingParams(rawQuery string) string {
	var kept []string
	for _, param := range strings.Split(rawQuery, "&") {
		key, _, _ := strings.Cut(param, "=")
		if param == "" || strings.HasPrefix(strings.ToLower(key), "utm_") {
			continue
		}
		kept = append(kept, param)
	}
	return strings.Join(kept, "&")
}

// PublisherDomain follows the Public Suffix List: uk.reuters.com becomes reuters.com.
func PublisherDomain(normalizedURL string) string {
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return domain
}
