package leverprice

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

type requestInfo struct {
	cookieInput       string
	assignmentCookies []string
	pinned            bool
	userAgent         string
	prefetch          bool
}

func readRequest(pr *PriceRequest, cookieName string) requestInfo {
	h := pr.Header
	if h == nil && pr.Request != nil {
		h = pr.Request.Header
	}
	var info requestInfo
	if pr.Cookie != "" {
		info.cookieInput = pr.Cookie
	} else {
		info.cookieInput, _ = headerValue(h, "cookie")
	}
	if pr.Cookie != "" && !strings.Contains(pr.Cookie, "=") {
		if v := jsTrim(pr.Cookie); v != "" {
			info.assignmentCookies = []string{v}
		}
	} else {
		info.assignmentCookies = cookieValues(info.cookieInput, cookieName)
		info.pinned = len(cookieValues(info.cookieInput, cookieName+"_f")) > 0
	}
	if pr.UserAgent != "" {
		info.userAgent = pr.UserAgent
	} else {
		info.userAgent, _ = headerValue(h, "user-agent")
	}
	info.prefetch = isPrefetch(h)
	return info
}

func headerValue(h http.Header, name string) (string, bool) {
	if h == nil {
		return "", false
	}
	values, ok := h[http.CanonicalHeaderKey(name)]
	if !ok {
		for k, v := range h {
			if strings.EqualFold(k, name) {
				values, ok = v, true
				break
			}
		}
	}
	if !ok || len(values) == 0 {
		return "", false
	}
	sep := ", "
	if strings.EqualFold(name, "cookie") {
		sep = "; "
	}
	return strings.Join(values, sep), true
}

// cookieValues also splits on "," because proxies join repeated Cookie headers with ", ".
func cookieValues(header, name string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(header, func(r rune) bool { return r == ';' || r == ',' }) {
		eq := strings.IndexByte(part, '=')
		if eq <= 0 || jsTrim(part[:eq]) != name {
			continue
		}
		value := jsTrim(part[eq+1:])
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if strings.IndexByte(value, '%') >= 0 {
			if decoded, err := url.PathUnescape(value); err == nil && utf8.ValidString(decoded) {
				value = decoded
			}
		}
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func isPrefetch(h http.Header) bool {
	for _, name := range [...]string{"sec-purpose", "purpose", "x-purpose", "x-moz"} {
		if v, ok := headerValue(h, name); ok {
			v = asciiLower(v)
			if strings.Contains(v, "prefetch") || strings.Contains(v, "prerender") || strings.Contains(v, "preview") {
				return true
			}
		}
	}
	v, ok := headerValue(h, "next-router-prefetch")
	return ok && v == "1"
}
