package leverprice

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// visitorSetHeader reports whether every browser or client sends the header itself. Such a header
// is never a source for the visitor's country, since a visitor could pick their own price with it.
// The dashboard applies the same rule.
func visitorSetHeader(name string) bool {
	lower := asciiLower(name)
	for _, prefix := range [...]string{"sec-", "accept", "content-", "if-", "x-forwarded-", "proxy-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	switch lower {
	case "authorization", "cache-control", "connection", "cookie", "dnt", "forwarded", "host", "origin",
		"pragma", "priority", "purpose", "range", "referer", "te", "upgrade-insecure-requests", "user-agent",
		"via", "x-real-ip", "x-requested-with":
		return true
	}
	return false
}

// addressHeaders are set by proxies and hosts to the visitor's IP address, in the order they are
// read. Only a visitor without a cookie is told apart by it, so a forged one picks no more than
// clearing cookies.
var addressHeaders = [...]string{
	"cf-connecting-ip",
	"true-client-ip",
	"fastly-client-ip",
	"fly-client-ip",
	"x-real-ip",
	"x-forwarded-for",
}

// clientAddress returns the visitor's address: the first address header that holds one, else the
// connection's.
func clientAddress(pr *PriceRequest) (string, bool) {
	h := requestHeader(pr)
	for _, name := range addressHeaders {
		value, ok := headerValue(h, name)
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(value, ",")
		if address, ok := normalizeAddress(first); ok {
			return address, true
		}
	}
	if pr.Request == nil {
		return "", false
	}
	return normalizeAddress(pr.Request.RemoteAddr)
}

// normalizeAddress spells an address one way: IPv4 in decimal ("203.0.113.7"), IPv6 cut to its
// /64 network as four lowercase hex groups ("2001:db8:0:1"), since phones change the rest of it
// every day. It accepts a port ("203.0.113.7:443", "[2001:db8::1]:443"), brackets and a zone
// ("%eth0"). It parses by hand: net/netip accepts spellings the other SDKs refuse.
func normalizeAddress(value string) (string, bool) {
	s := strings.Trim(value, " \t")
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 || (end+1 < len(s) && !isPort(s[end+1:])) {
			return "", false
		}
		s = s[1:end]
	} else if colon := strings.IndexByte(s, ':'); colon > 0 && isDigitsAndDots(s[:colon]) && isPort(s[colon:]) {
		s = s[:colon]
	}
	if zone := strings.IndexByte(s, '%'); zone >= 0 {
		s = s[:zone]
	}
	if v4, ok := parseIPv4(s); ok {
		return fmt.Sprintf("%d.%d.%d.%d", v4[0], v4[1], v4[2], v4[3]), true
	}
	v6, ok := parseIPv6(s)
	if !ok {
		return "", false
	}
	if v6[0]|v6[1]|v6[2]|v6[3]|v6[4] == 0 && v6[5] == 0xffff {
		return fmt.Sprintf("%d.%d.%d.%d", v6[6]>>8, v6[6]&0xff, v6[7]>>8, v6[7]&0xff), true
	}
	return fmt.Sprintf("%x:%x:%x:%x", v6[0], v6[1], v6[2], v6[3]), true
}

// isPort matches ":" and 1 to 5 digits.
func isPort(s string) bool {
	return len(s) >= 2 && len(s) <= 6 && s[0] == ':' && isDigits(s[1:])
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isDigitsAndDots(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return s != ""
}

// parseIPv4 reads four decimal numbers 0-255 joined by dots, without a sign or a leading zero.
func parseIPv4(s string) ([4]int, bool) {
	var out [4]int
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return out, false
	}
	for i, part := range parts {
		if !isDigits(part) || len(part) > 3 || (len(part) > 1 && part[0] == '0') {
			return out, false
		}
		n, _ := strconv.Atoi(part)
		if n > 255 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// parseIPv6 reads 8 groups of 1-4 hex digits, at most one "::" standing for one or more zero
// groups, optionally ending in an IPv4 address that stands for the last two groups.
func parseIPv6(text string) ([8]uint16, bool) {
	var out [8]uint16
	s := text
	if strings.Contains(s, ".") {
		colon := strings.LastIndexByte(s, ':')
		if colon < 0 {
			return out, false
		}
		v4, ok := parseIPv4(s[colon+1:])
		if !ok {
			return out, false
		}
		s = fmt.Sprintf("%s%x:%x", s[:colon+1], v4[0]<<8|v4[1], v4[2]<<8|v4[3])
	}
	halves := strings.Split(s, "::")
	if len(halves) > 2 {
		return out, false
	}
	var head, tail []string
	if halves[0] != "" {
		head = strings.Split(halves[0], ":")
	}
	compressed := len(halves) == 2
	if compressed && halves[1] != "" {
		tail = strings.Split(halves[1], ":")
	}
	groups := len(head) + len(tail)
	if compressed && groups > 7 || !compressed && groups != 8 {
		return out, false
	}
	parse := func(list []string, at int) bool {
		for i, g := range list {
			if len(g) < 1 || len(g) > 4 {
				return false
			}
			n, err := strconv.ParseUint(g, 16, 16)
			if err != nil {
				return false
			}
			out[at+i] = uint16(n)
		}
		return true
	}
	return out, parse(head, 0) && parse(tail, 8-len(tail))
}
