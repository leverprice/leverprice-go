package leverprice

import (
	"crypto/hmac"
	"strconv"
	"strings"
)

const (
	maxCookieExperiments = 10
	maxCookieLength      = 4096
	pinMaxAgeSeconds     = 1800
)

type assignmentPair struct {
	experimentID string
	variantID    string
}

// assignmentState keeps assignments as ordered pairs: the cookie lists them most recent first.
type assignmentState struct {
	v string
	a []assignmentPair
	x []string
}

func newAssignmentState(v string) *assignmentState {
	return &assignmentState{v: v, a: []assignmentPair{}, x: []string{}}
}

func (s *assignmentState) clone() *assignmentState {
	return &assignmentState{
		v: s.v,
		a: append([]assignmentPair{}, s.a...),
		x: append([]string{}, s.x...),
	}
}

func (s *assignmentState) variantOf(experimentID string) (string, bool) {
	for _, p := range s.a {
		if p.experimentID == experimentID {
			return p.variantID, true
		}
	}
	return "", false
}

func (s *assignmentState) assigned(experimentID string) bool {
	_, ok := s.variantOf(experimentID)
	return ok
}

func (s *assignmentState) exposed(experimentID string) bool {
	for _, id := range s.x {
		if id == experimentID {
			return true
		}
	}
	return false
}

func (s *assignmentState) keepAssignedExposures(limit int) {
	x := make([]string, 0, len(s.x))
	for _, id := range s.x {
		if s.assigned(id) && len(x) < limit {
			x = append(x, id)
		}
	}
	s.x = x
}

func (s *assignmentState) assign(experimentID, variantID string) {
	a := make([]assignmentPair, 0, len(s.a)+1)
	a = append(a, assignmentPair{experimentID, variantID})
	for _, p := range s.a {
		if p.experimentID != experimentID {
			a = append(a, p)
		}
	}
	if len(a) > maxCookieExperiments {
		a = a[:maxCookieExperiments]
	}
	s.a = a
	s.keepAssignedExposures(len(s.x))
}

func (s *assignmentState) markExposed(experimentID string) bool {
	if s.exposed(experimentID) {
		return false
	}
	s.x = append([]string{experimentID}, s.x...)
	s.keepAssignedExposures(maxCookieExperiments)
	return true
}

func (s *assignmentState) json() string {
	b := make([]byte, 0, 64+48*len(s.a))
	b = append(b, `{"v":`...)
	b = appendJSONString(b, s.v)
	b = append(b, `,"a":{`...)
	for i, p := range s.a {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, p.experimentID)
		b = append(b, ':')
		b = appendJSONString(b, p.variantID)
	}
	b = append(b, `},"x":[`...)
	for i, id := range s.x {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, id)
	}
	return string(append(b, "]}"...))
}

func cookieTag(key []byte, body string) []byte {
	return hmacSHA256(key, "pp:cookie:v1:"+body)[:16]
}

func signAssignment(key []byte, s *assignmentState) string {
	body := b64url([]byte(s.json()))
	return body + "." + b64url(cookieTag(key, body))
}

func verifyAssignment(key []byte, raw string) *assignmentState {
	if len(raw) > maxCookieLength {
		return nil
	}
	dot := strings.IndexByte(raw, '.')
	if dot <= 0 {
		return nil
	}
	body, sig := raw[:dot], raw[dot+1:]
	if !isB64url(body) || len(sig) != 22 || !isB64url(sig) {
		return nil
	}
	got, err := fromB64url(sig)
	if err != nil || !hmac.Equal(cookieTag(key, body), got) {
		return nil
	}
	data, err := decodeJSONPayload(body)
	if err != nil {
		return nil
	}
	return normalizeState(data)
}

func normalizeState(raw []byte) *assignmentState {
	obj, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	v, ok := jsonString(obj["v"])
	if !ok || !isLowerHex(v, 32) {
		return nil
	}
	members, ok := jsonOrderedObject(obj["a"])
	if !ok {
		return nil
	}
	s := newAssignmentState(v)
	if len(members) > maxCookieExperiments {
		members = members[:maxCookieExperiments]
	}
	for _, m := range members {
		if variantID, ok := jsonString(m.value); ok && isID(m.key) && isID(variantID) {
			s.a = append(s.a, assignmentPair{m.key, variantID})
		}
	}
	if list, ok := jsonArray(obj["x"]); ok {
		for _, item := range list {
			if id, ok := jsonString(item); ok && s.assigned(id) && len(s.x) < maxCookieExperiments {
				s.x = append(s.x, id)
			}
		}
	}
	return s
}

func setCookieHeader(name, value string, maxAgeSeconds int64, secure bool, domain string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteString("; Path=/; Max-Age=")
	b.WriteString(strconv.FormatInt(maxAgeSeconds, 10))
	b.WriteString("; SameSite=Lax; HttpOnly")
	if secure {
		b.WriteString("; Secure")
	}
	if domain != "" {
		b.WriteString("; Domain=")
		b.WriteString(domain)
	}
	return b.String()
}
