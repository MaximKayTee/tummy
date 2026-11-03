package config

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	secretParam = regexp.MustCompile(`(?i)^(password|passwd|pwd|pass|secret|token|access_token|api_key|apikey|key|sig|signature|client_secret)$`)
	secretPair  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass|secret|token|api_key|apikey|client_secret)(\s*=\s*)('[^']*'|"[^"]*"|\{[^}]*\}|[^\s;&]*)`)
)

// Redact masks passwords and secret parameters in a connection string: user:pass@ in URLs
// and go-sql-driver DSNs, ?password= style query parameters, and password=... in
// key=value DSNs.
func Redact(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		start := i + 3
		end := len(s)
		if u, err := url.Parse(s); err == nil {
			// a parsed URL has no raw / ? or # in its userinfo
			if j := strings.IndexAny(s[start:], "/?#"); j >= 0 {
				end = start + j
			}
			if _, ok := u.User.Password(); !ok {
				end = start
			}
		} else if j := strings.IndexByte(s[start:], '?'); j >= 0 {
			end = start + j
		}
		return maskQuery(maskUserinfo(s, start, end))
	}
	if at := strings.LastIndexByte(s, '@'); at > 0 && !strings.ContainsAny(s[:at], " ;=") {
		// user:pass@tcp(host)/db: the userinfo ends at the last @ before the last /
		end := len(s)
		if j := strings.LastIndexByte(s, '/'); j > at {
			end = j
		}
		s = maskUserinfo(s, 0, end)
	}
	return secretPair.ReplaceAllString(s, "$1$2***")
}

// maskUserinfo masks the password in the user:pass@ that ends at the last @ in s[start:end].
func maskUserinfo(s string, start, end int) string {
	at := strings.LastIndexByte(s[start:end], '@')
	if at < 0 {
		return s
	}
	at += start
	colon := strings.IndexByte(s[start:at], ':')
	if colon < 0 {
		return s
	}
	return s[:start+colon+1] + "***" + s[at:]
}

// maskQuery masks the values of secret query parameters.
func maskQuery(s string) string {
	base, query, ok := strings.Cut(s, "?")
	if !ok {
		return s
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if dk, err := url.QueryUnescape(k); err == nil && secretParam.MatchString(dk) {
			parts[i] = k + "=***"
		}
	}
	return base + "?" + strings.Join(parts, "&")
}
