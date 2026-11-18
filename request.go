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
	country           string
}

func readRequest(pr *PriceRequest, cookieName, countryHeader string) requestInfo {
	h := requestHeader(pr)
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
	switch {
	case pr.Country != "":
		info.country = normalizeCountry(pr.Country)
	case countryHeader != "":
		info.country = headerCountry(h, countryHeader)
	}
	return info
}

func requestHeader(pr *PriceRequest) http.Header {
	if pr.Header == nil && pr.Request != nil {
		return pr.Request.Header
	}
	return pr.Header
}

func headerCountry(h http.Header, name string) string {
	value, _ := headerValue(h, name)
	return normalizeCountry(value)
}

// normalizeCountry returns the ISO 3166-1 alpha-2 code in value, or "" for anything else,
// including what CDNs send for an unknown location, Tor, anonymous proxies and regions. It reads
// ASCII bytes only: Unicode trimming or case mapping would turn other input into a country.
func normalizeCountry(value string) string {
	code := trimASCIISpace(value)
	if len(code) != 2 || !isASCIILetter(code[0]) || !isASCIILetter(code[1]) {
		return ""
	}
	code = string([]byte{code[0] &^ 0x20, code[1] &^ 0x20})
	switch code {
	case "XX", "ZZ", "T1", "A1", "A2", "AP", "EU":
		return ""
	}
	return code
}

func headerValue(h http.Header, name string) (string, bool) {
	if h == nil {
		return "", false
	}
	values, ok := h[http.CanonicalHeaderKey(name)]
	if !ok {
		lower := asciiLower(name)
		for k, v := range h {
			if asciiLower(k) == lower {
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
	for _, name := range [...]string{"sec-purpose", "x-sec-purpose", "purpose", "x-purpose", "x-moz"} {
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
