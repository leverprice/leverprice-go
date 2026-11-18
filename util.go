package leverprice

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSafeInteger = 1<<53 - 1

var errBase64 = errors.New("invalid base64url")

func isSafeInteger(f float64) bool {
	return isFinite(f) && f == math.Trunc(f) && math.Abs(f) <= maxSafeInteger
}

func isFinite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }

func isB64urlByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

func isB64url(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isB64urlByte(s[i]) {
			return false
		}
	}
	return true
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// fromB64url checks the alphabet first: the standard decoder silently skips CR and LF.
func fromB64url(s string) ([]byte, error) {
	if !isB64url(s) {
		return nil, errBase64
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func hmacHex(key []byte, msg string, chars int) string {
	return hex.EncodeToString(hmacSHA256(key, msg))[:chars]
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ed25519Verify(publicKey, signature, data string) bool {
	pub, err := fromB64url(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := fromB64url(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(data), sig)
}

func bucketFor(salt, visitorID string) int {
	h := sha256.Sum256([]byte(salt + ":" + visitorID))
	n := uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
	return int(n % 10000)
}

func pickVariant(weights []float64, bucket int) int {
	acc := 0.0
	for i, w := range weights {
		acc += w
		if float64(bucket) < acc {
			return i
		}
	}
	return len(weights) - 1
}

func isID(s string) bool {
	return len(s) >= 1 && len(s) <= 64 && isB64url(s)
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func isLowerCurrencyCode(s string) bool {
	return isCurrencyCode(s) && asciiLower(s) == s
}

func isCountryCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// isJSSpace is JavaScript's whitespace set, used by every SDK for trimming and URL checks.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// trimASCIISpace strips space, tab, LF, VT, FF and CR only. Where a value selects a price or names
// a header, U+00A0 and other Unicode whitespace must stay in place and fail validation.
func trimASCIISpace(s string) string { return strings.Trim(s, " \t\n\v\f\r") }

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// asciiLower folds only A-Z: Unicode folding would also map "ſ" to "s" and "K" to "k".
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func httpsURL(s string) string {
	const scheme = "https://"
	if len(s) <= len(scheme) || asciiLower(s[:len(scheme)]) != scheme {
		return ""
	}
	if strings.IndexFunc(s[len(scheme):], isJSSpace) >= 0 {
		return ""
	}
	return s
}

// utf16Len counts UTF-16 code units, the unit of the server's length limits.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Width(r)
	}
	return n
}

func utf16Width(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

func truncateUTF16(s string, limit int) string {
	n := 0
	for i, r := range s {
		if n+utf16Width(r) > limit {
			return s[:i]
		}
		n += utf16Width(r)
	}
	return s
}

func stripBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")) }

type jsonMember struct {
	key   string
	value json.RawMessage
}

func jsonKind(raw json.RawMessage) byte {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	if len(raw) == 0 {
		return 0
	}
	return raw[0]
}

func isJSONNull(raw json.RawMessage) bool { return jsonKind(raw) == 'n' }

func jsonString(raw json.RawMessage) (string, bool) {
	if jsonKind(raw) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func jsonNumber(raw json.RawMessage) (float64, bool) {
	if k := jsonKind(raw); k != '-' && (k < '0' || k > '9') {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(raw)), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

func jsonSafeInt(raw json.RawMessage) (int64, bool) {
	f, ok := jsonNumber(raw)
	if !ok || !isSafeInteger(f) {
		return 0, false
	}
	return int64(f), true
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if jsonKind(raw) != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func jsonArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if jsonKind(raw) != '[' {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	return a, true
}

// jsonOrderedObject returns members in document order; a repeated key keeps its first position
// and its last value, as in JSON.parse.
func jsonOrderedObject(raw json.RawMessage) ([]jsonMember, bool) {
	if jsonKind(raw) != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var members []jsonMember
	index := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		if i, seen := index[key]; seen {
			members[i].value = value
			continue
		}
		index[key] = len(members)
		members = append(members, jsonMember{key, value})
	}
	return members, true
}

func decodeJSONPayload(payload string) (json.RawMessage, error) {
	b, err := fromB64url(payload)
	if err != nil {
		return nil, err
	}
	b = stripBOM(b)
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	return json.RawMessage(b), nil
}

// appendJSONString encodes like JSON.stringify: unlike encoding/json it leaves <, >, & and
// U+2028/U+2029 as they are, so signed cookie bodies match the other SDKs byte for byte.
func appendJSONString(b []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			b = utf8.AppendRune(b, r)
			i += size
			continue
		}
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c >= 0x20:
			b = append(b, c)
		case c == '\b':
			b = append(b, '\\', 'b')
		case c == '\f':
			b = append(b, '\\', 'f')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
		i++
	}
	return append(b, '"')
}
