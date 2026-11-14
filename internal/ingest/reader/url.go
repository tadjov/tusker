package reader

import (
	"net/url"
	"strings"

	"github.com/tadjov/tusker/pkg/apis/datamapping"
	"golang.org/x/net/publicsuffix"
)

// NormalizeURL trims and validates raw, returning a cleaned absolute http(s)
// URL. The bool is false when raw is empty or not a valid http(s) URL.
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
	return u.String(), true
}

// URLIdentity is the name an article id hashes: normalizedURL itself without
// identity rules, or with the parts they ignore taken out.
func URLIdentity(normalizedURL string, identity *datamapping.Identity) string {
	if identity == nil {
		return normalizedURL
	}
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return normalizedURL
	}
	ignore := identity.Ignore
	if ignore.WWW {
		u.Host = strings.TrimPrefix(u.Host, "www.")
	}
	if ignore.TrailingSlash {
		u.Path = strings.TrimSuffix(u.Path, "/")
		u.RawPath = strings.TrimSuffix(u.RawPath, "/")
	}
	if ignore.Fragment {
		u.Fragment, u.RawFragment = "", ""
	}
	if len(ignore.QueryParams) > 0 {
		u.RawQuery = withoutIgnoredParams(u.RawQuery, ignore)
	}
	name := u.String()
	if ignore.Scheme {
		name = strings.TrimPrefix(name, u.Scheme+"://")
	}
	return name
}

// withoutIgnoredParams edits the raw query, since url.Values would re-sort and re-encode it.
func withoutIgnoredParams(rawQuery string, ignore datamapping.IdentityIgnore) string {
	var kept []string
	for _, param := range strings.Split(rawQuery, "&") {
		key, _, _ := strings.Cut(param, "=")
		if param == "" || ignore.IgnoresQueryParam(key) {
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
