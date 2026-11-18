package leverprice

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func assignmentKey(t *testing.T) []byte {
	t.Helper()
	key, err := fromB64url(vectors(t).AssignmentKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func sameState(got *assignmentState, want *vectorState) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	w := want.state()
	return got.v == w.v && reflect.DeepEqual(got.a, w.a) && reflect.DeepEqual(got.x, w.x)
}

func TestVectorKeyFingerprints(t *testing.T) {
	for _, tc := range vectors(t).KeyFingerprints {
		if got := sha256Hex(tc.SecretKey)[:16]; got != tc.Fingerprint {
			t.Errorf("fingerprint(%s) = %s, want %s", tc.SecretKey, got, tc.Fingerprint)
		}
	}
}

func TestVectorTrust(t *testing.T) {
	v := vectors(t)
	for _, tc := range v.Trust {
		t.Run(tc.Name, func(t *testing.T) {
			c, err := New(Options{SecretKey: v.SecretKey, TrustedRootKeys: v.RootKeys, OnError: func(error) {}})
			if err != nil {
				t.Fatal(err)
			}
			minVersion := int64(MinKeysetVersion)
			if tc.MinKeysetVersion != nil {
				minVersion = *tc.MinKeysetVersion
			}
			c.trust = newTrustChain(v.RootKeys, minVersion)
			for i, step := range tc.Steps {
				now := v.Now
				if step.Now != nil {
					now = *step.Now
				}
				c.now = func() time.Time { return time.UnixMilli(now) }
				dropped, _, err := c.storeConfig(step.Envelope, `"x"`)
				c.mu.Lock()
				version := c.loadedVersionLocked()
				countryHeader := ""
				if c.config != nil {
					countryHeader = c.config.countryHeader
				}
				// What the next config request reports in Leverprice-Config-Error.
				refusal := c.configError
				c.mu.Unlock()
				switch step.Expect {
				case "accept":
					if err != nil {
						t.Fatalf("step %d: rejected: %v", i, err)
					}
					if version != step.Version {
						t.Errorf("step %d: version %d, want %d", i, version, step.Version)
					}
					if !reflect.DeepEqual(dropped, step.Dropped) {
						t.Errorf("step %d: dropped %v, want %v", i, dropped, step.Dropped)
					}
					if step.CountryHeader != nil {
						var want *string
						if err := json.Unmarshal(step.CountryHeader, &want); err != nil {
							t.Fatal(err)
						}
						if countryHeader != deref(want) {
							t.Errorf("step %d: country header %q, want %q", i, countryHeader, deref(want))
						}
					}
					if refusal != "" {
						t.Errorf("step %d: still reports the refusal %q", i, refusal)
					}
				case "reject":
					if err == nil {
						t.Fatalf("step %d: accepted, want reject", i)
					}
					if !strings.HasPrefix(err.Error(), "leverprice") {
						t.Errorf("step %d: error %q lacks the leverprice prefix", i, err)
					}
					if refusal != step.Code {
						t.Errorf("step %d: refusal code %q, want %q (%v)", i, refusal, step.Code, err)
					}
				default:
					t.Fatalf("step %d: unknown expectation %q", i, step.Expect)
				}
			}
		})
	}
}

func TestVectorVisitorIDs(t *testing.T) {
	key := assignmentKey(t)
	for _, tc := range vectors(t).VisitorIDs {
		if msg := "pp:visitor:v1:" + tc.VisitorKey; msg != tc.Message {
			t.Errorf("message %q, want %q", msg, tc.Message)
		}
		if got := hmacHex(key, tc.Message, 32); got != tc.VisitorID {
			t.Errorf("visitorId(%s) = %s, want %s", tc.VisitorKey, got, tc.VisitorID)
		}
	}
}

func TestVectorAttribution(t *testing.T) {
	key := assignmentKey(t)
	for _, tc := range vectors(t).Attribution {
		msg := "pp:attr:v1:" + tc.ExperimentID + ":" + tc.VariantID + ":" + tc.VisitorID
		if msg != tc.Message {
			t.Errorf("message %q, want %q", msg, tc.Message)
		}
		if got := hmacHex(key, msg, 16); got != tc.Signature {
			t.Errorf("signature(%s) = %s, want %s", msg, got, tc.Signature)
		}
	}
}

func TestVectorBuckets(t *testing.T) {
	for _, tc := range vectors(t).Buckets {
		if got := bucketFor(tc.Salt, tc.VisitorID); got != tc.Bucket {
			t.Errorf("bucket(%q, %q) = %d, want %d", tc.Salt, tc.VisitorID, got, tc.Bucket)
		}
	}
}

func TestVectorPickVariant(t *testing.T) {
	for _, tc := range vectors(t).PickVariant {
		if got := pickVariant(tc.Weights, tc.Bucket); got != tc.Index {
			t.Errorf("pickVariant(%v, %d) = %d, want %d", tc.Weights, tc.Bucket, got, tc.Index)
		}
	}
}

func TestVectorCookieSign(t *testing.T) {
	key := assignmentKey(t)
	for _, tc := range vectors(t).Cookies.Sign {
		s := tc.State.state()
		if got := s.json(); got != tc.JSON {
			t.Errorf("json = %s, want %s", got, tc.JSON)
		}
		if got := signAssignment(key, s); got != tc.Cookie {
			t.Errorf("cookie = %s, want %s", got, tc.Cookie)
		}
	}
}

func TestVectorCookieVerify(t *testing.T) {
	key := assignmentKey(t)
	for _, tc := range vectors(t).Cookies.Verify {
		t.Run(tc.Name, func(t *testing.T) {
			if got := verifyAssignment(key, tc.Cookie); !sameState(got, tc.State) {
				t.Errorf("verify = %+v, want %+v", got, tc.State)
			}
		})
	}
}

func TestVectorCookieOps(t *testing.T) {
	for _, tc := range vectors(t).Cookies.Ops {
		t.Run(tc.Name, func(t *testing.T) {
			s := tc.State.state()
			switch {
			case tc.Op.Assign != nil:
				s.assign(tc.Op.Assign[0], tc.Op.Assign[1])
			case tc.Op.MarkExposed != nil:
				changed := s.markExposed(*tc.Op.MarkExposed)
				if tc.Changed != nil && changed != *tc.Changed {
					t.Errorf("changed = %v, want %v", changed, *tc.Changed)
				}
			default:
				t.Fatal("unknown op")
			}
			if !sameState(s, &tc.Result) {
				t.Errorf("result = %+v, want %+v", s, tc.Result)
			}
		})
	}
}

func TestVectorCookieHeader(t *testing.T) {
	for _, tc := range vectors(t).CookieHeader {
		got := cookieValues(tc.Header, tc.Name)
		if len(got) != len(tc.Values) || (len(got) > 0 && !reflect.DeepEqual(got, tc.Values)) {
			t.Errorf("cookieValues(%q, %q) = %q, want %q", tc.Header, tc.Name, got, tc.Values)
		}
	}
}

func TestVectorPrefetch(t *testing.T) {
	for _, tc := range vectors(t).Prefetch {
		if got := isPrefetch(headerOf(tc.Headers)); got != tc.Prefetch {
			t.Errorf("isPrefetch(%v) = %v, want %v", tc.Headers, got, tc.Prefetch)
		}
	}
}

func TestVectorSetCookie(t *testing.T) {
	for _, tc := range vectors(t).SetCookie {
		domain := ""
		if tc.Domain != nil {
			domain = *tc.Domain
		}
		if got := setCookieHeader(tc.Name, tc.Value, tc.MaxAge, tc.Secure, domain); got != tc.Header {
			t.Errorf("setCookie = %s, want %s", got, tc.Header)
		}
	}
}

func TestVectorCountries(t *testing.T) {
	for _, tc := range vectors(t).Countries {
		if got := normalizeCountry(tc.Value); got != deref(tc.Country) {
			t.Errorf("normalizeCountry(%q) = %q, want %q", tc.Value, got, deref(tc.Country))
		}
	}
}

func TestVectorBots(t *testing.T) {
	for _, tc := range vectors(t).Bots {
		if got := IsBot(tc.UserAgent); got != tc.Bot {
			t.Errorf("IsBot(%q) = %v, want %v", tc.UserAgent, got, tc.Bot)
		}
	}
}

func TestVectorEvents(t *testing.T) {
	for _, tc := range vectors(t).Events {
		e, ok := eventFromJSON(t, tc.Event)
		if !ok {
			if tc.Valid {
				t.Errorf("valid event %s does not fit the Go types", tc.Event)
			}
			continue
		}
		if problem := eventProblem(e); (problem == "") != tc.Valid {
			t.Errorf("eventProblem(%s) = %q, want valid=%v", tc.Event, problem, tc.Valid)
		}
	}
}

func eventFromJSON(t *testing.T, raw json.RawMessage) (wireEvent, bool) {
	t.Helper()
	obj, ok := jsonObject(raw)
	if !ok {
		t.Fatalf("event %s is not an object", raw)
	}
	str := func(m map[string]json.RawMessage, k string) string {
		s, _ := jsonString(m[k])
		return s
	}
	fits := true
	integer := func(m map[string]json.RawMessage, k string) int64 {
		f, ok := jsonNumber(m[k])
		if !ok || f != math.Trunc(f) || math.Abs(f) > math.MaxInt64/2 {
			fits = false
			return 0
		}
		return int64(f)
	}
	optional := func(m map[string]json.RawMessage, k string) *int64 {
		if _, present := m[k]; !present {
			return nil
		}
		n := integer(m, k)
		return &n
	}
	switch str(obj, "type") {
	case "exposure":
		e := exposureEvent{
			Type:         "exposure",
			ExperimentID: str(obj, "experimentId"),
			VariantID:    str(obj, "variantId"),
			VisitorID:    str(obj, "visitorId"),
			Timestamp:    integer(obj, "timestamp"),
		}
		if raw, present := obj["country"]; present {
			country, ok := jsonString(raw)
			e.Country = country
			fits = fits && ok && country != ""
		}
		return e, fits
	case "purchase":
		e := purchaseEvent{
			Type:      "purchase",
			Provider:  Provider(str(obj, "provider")),
			OrderID:   str(obj, "orderId"),
			Amount:    integer(obj, "amount"),
			Currency:  str(obj, "currency"),
			Timestamp: integer(obj, "timestamp"),
		}
		if a, ok := jsonObject(obj["attribution"]); ok {
			e.Attribution = &wireAttribution{ExperimentID: str(a, "experimentId"), VariantID: str(a, "variantId"), VisitorID: str(a, "visitorId")}
			if sig, ok := jsonString(a["signature"]); ok {
				e.Attribution.Signature = &sig
			}
		}
		return e, fits
	case "refund":
		e := refundEvent{
			Type:          "refund",
			Provider:      Provider(str(obj, "provider")),
			OrderID:       str(obj, "orderId"),
			Currency:      str(obj, "currency"),
			Timestamp:     integer(obj, "timestamp"),
			Amount:        optional(obj, "amount"),
			RefundedTotal: optional(obj, "refundedTotal"),
		}
		if id, ok := jsonString(obj["refundId"]); ok {
			e.RefundID = &id
		}
		return e, fits
	}
	t.Fatalf("unknown event type in %s", raw)
	return nil, false
}

type quoteExpectation struct {
	Price          int64             `json:"price"`
	Currency       string            `json:"currency"`
	Country        *string           `json:"country"`
	Regional       bool              `json:"regional"`
	IsFallback     bool              `json:"isFallback"`
	Reason         Reason            `json:"reason"`
	ExperimentID   *string           `json:"experimentId"`
	VariantID      *string           `json:"variantId"`
	VisitorID      *string           `json:"visitorId"`
	Metadata       map[string]string `json:"metadata"`
	CheckoutURL    *string           `json:"checkoutUrl"`
	ExposureLogged bool              `json:"exposureLogged"`
	SetCookie      []string          `json:"setCookie"`
	Exposures      []map[string]any  `json:"exposures"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestVectorQuotes(t *testing.T) {
	for _, tc := range vectors(t).Quotes {
		t.Run(tc.Name, func(t *testing.T) {
			var want quoteExpectation
			if err := json.Unmarshal(tc.Expect, &want); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, func(o *Options) {
				for k, g := range tc.Guards {
					o.Products[k] = ProductGuard(g)
				}
				o.CountryHeader = tc.CountryHeader
				o.LocalCurrencies = tc.LocalCurrencies
			})
			req := PriceRequest{
				Header:  headerOf(tc.Headers),
				UserID:  deref(tc.UserID),
				Country: deref(tc.Country),
				Bot:     tc.Bot,
			}
			if tc.Exposure == "manual" {
				req.Exposure = ExposureManual
			}
			q := h.price(t, tc.ProductKey, req)
			h.c.Flush(context.Background())

			got := quoteExpectation{
				Price:          q.Price,
				Currency:       q.Currency,
				Country:        nullable(q.Country),
				Regional:       q.Regional,
				IsFallback:     q.IsFallback,
				Reason:         q.Reason,
				ExperimentID:   nullable(q.ExperimentID),
				VariantID:      nullable(q.VariantID),
				VisitorID:      nullable(q.VisitorID),
				Metadata:       q.Metadata,
				CheckoutURL:    nullable(q.CheckoutURL),
				ExposureLogged: q.ExposureLogged,
				SetCookie:      q.SetCookie,
				Exposures:      []map[string]any{},
			}
			for _, e := range h.api.sentEvents(t) {
				delete(e, "timestamp")
				got.Exposures = append(got.Exposures, e)
			}
			if !reflect.DeepEqual(normalize(t, got), normalize(t, want)) {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(want)
				t.Errorf("quote\n got %s\nwant %s", gj, wj)
			}
			if q.ProductKey != tc.ProductKey {
				t.Errorf("productKey = %q, want %q", q.ProductKey, tc.ProductKey)
			}
			if errs := h.errs.unexpected(); !slices.Equal(errs, wantErrors(tc.Errors)) {
				t.Errorf("errors\n got %q\nwant %q", errs, wantErrors(tc.Errors))
			}
			var asJSON, expected map[string]any
			b, _ := json.Marshal(q)
			_ = json.Unmarshal(b, &asJSON)
			_ = json.Unmarshal(tc.Expect, &expected)
			delete(expected, "exposures")
			delete(asJSON, "productKey")
			if !reflect.DeepEqual(asJSON, expected) {
				t.Errorf("JSON\n got %s\nwant %s", b, tc.Expect)
			}
		})
	}
}

// wantErrors returns the vectors' messages as this package words them: with a lowercase prefix.
func wantErrors(messages []string) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		if rest, ok := strings.CutPrefix(m, "Leverprice"); ok {
			m = "leverprice" + rest
		}
		out = append(out, m)
	}
	return out
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVectorCountryHeaderNames(t *testing.T) {
	for _, tc := range vectors(t).CountryHeaderNames {
		if allowed := !visitorSetHeader(tc.Name); allowed != tc.Allowed {
			t.Errorf("%q allowed = %v, want %v", tc.Name, allowed, tc.Allowed)
		}
	}
}

func TestVectorAddresses(t *testing.T) {
	for _, tc := range vectors(t).Addresses {
		value, isString := jsonString(tc.Value)
		got, ok := normalizeAddress(value)
		if !isString {
			got, ok = "", false // Go takes only strings; anything else is no address
		}
		if want := deref(tc.Address); ok != (tc.Address != nil) || got != want {
			t.Errorf("normalizeAddress(%s) = %q, %v; want %q", tc.Value, got, ok, want)
		}
	}
	for _, tc := range vectors(t).ClientAddresses {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.Header = headerOf(tc.Headers)
		r.RemoteAddr = deref(tc.RemoteAddress)
		got, ok := clientAddress(&PriceRequest{Request: r})
		if want := deref(tc.Address); ok != (tc.Address != nil) || got != want {
			t.Errorf("clientAddress(%v, %q) = %q, %v; want %q", tc.Headers, r.RemoteAddr, got, ok, want)
		}
		got, ok = clientAddress(&PriceRequest{Header: headerOf(tc.Headers)})
		if tc.RemoteAddress == nil && (ok != (tc.Address != nil) || got != deref(tc.Address)) {
			t.Errorf("clientAddress(%v) without a request = %q, %v; want %q", tc.Headers, got, ok, deref(tc.Address))
		}
	}
}

func TestVectorAnonVisitors(t *testing.T) {
	key := assignmentKey(t)
	for _, tc := range vectors(t).AnonVisitors {
		msg := anonMessage(tc.Address, tc.UserAgent, tc.AcceptLanguage)
		if msg != tc.Message {
			t.Errorf("message %q, want %q", msg, tc.Message)
		}
		if got := hmacHex(key, msg, 32); got != tc.V {
			t.Errorf("visitor key for %q = %s, want %s", msg, got, tc.V)
		}
	}
}

func TestVectorProductsHeader(t *testing.T) {
	for _, tc := range vectors(t).ProductsHeader {
		h := newHarness(t, func(o *Options) {
			o.Products = map[string]ProductGuard{}
			for k, g := range tc.Products {
				o.Products[k] = ProductGuard(g)
			}
		})
		h.api.setConfig(status(http.StatusServiceUnavailable, ""))
		h.c.Refresh(context.Background())
		calls := h.api.calls("/v1/config")
		if len(calls) != 1 {
			t.Fatalf("%d config requests, want 1", len(calls))
		}
		if got, sent := calls[0].header[productsHeader]; !sent || !slices.Equal(got, []string{tc.Header}) {
			t.Errorf("Leverprice-Products %q (sent %v), want %q", got, sent, tc.Header)
		}
	}
}

func TestVectorCountryHold(t *testing.T) {
	hold := vectors(t).CountryHold
	key := assignmentKey(t)
	for _, tc := range hold.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.Products = map[string]ProductGuard{}
				for k, g := range hold.Guards {
					o.Products[k] = ProductGuard(g)
				}
				o.LocalCurrencies = tc.LocalCurrencies
			})
			h.api.setConfig(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v5"`)
				_, _ = w.Write(hold.Config)
			})
			now := h.clock.Now().Unix()
			header := http.Header{"User-Agent": {"Mozilla/5.0 Chrome/131.0"}}
			if tc.Country != nil {
				header.Set("X-Geo-Country", *tc.Country)
			}
			var signedAt *int64
			if tc.Cookie != nil {
				s := (&vectorState{V: hold.Visitor, A: tc.Cookie.A, X: tc.Cookie.X}).state()
				if tc.Cookie.C != nil && tc.Cookie.Age != nil {
					s.c, s.t = *tc.Cookie.C, now-*tc.Cookie.Age
					signedAt = &s.t
				}
				header.Set("Cookie", "__pp="+signAssignment(key, s))
			}
			q := h.price(t, tc.ProductKey, PriceRequest{Header: header})
			h.c.Flush(context.Background())

			exposures := []map[string]any{}
			for _, e := range h.api.sentEvents(t) {
				delete(e, "timestamp")
				delete(e, "visitorId")
				exposures = append(exposures, e)
			}
			var cookie any
			for _, set := range q.SetCookie {
				value, isAssignment := strings.CutPrefix(set, "__pp=")
				if !isAssignment {
					continue
				}
				value, _, _ = strings.Cut(value, ";")
				back := verifyAssignment(key, value)
				if back == nil {
					t.Fatalf("the quote's cookie does not verify: %s", set)
				}
				var held, since any
				if back.c != "" {
					held, since = back.c, "fresh"
					if signedAt != nil && back.t == *signedAt {
						since = "kept"
					} else if back.t < now-5 || back.t > now+5 {
						t.Errorf("hold stamped %d, now is %d", back.t, now)
					}
				}
				pairs := [][2]string{}
				for _, p := range back.a {
					pairs = append(pairs, [2]string{p.experimentID, p.variantID})
				}
				cookie = map[string]any{"c": held, "t": since, "a": pairs}
			}
			got := map[string]any{
				"price":     q.Price,
				"currency":  q.Currency,
				"country":   nullable(q.Country),
				"regional":  q.Regional,
				"reason":    q.Reason,
				"variantId": nullable(q.VariantID),
				"exposures": exposures,
				"cookie":    cookie,
			}
			var want any
			if err := json.Unmarshal(tc.Expect, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalize(t, got), want) {
				gj, _ := json.Marshal(got)
				t.Errorf("quote\n got %s\nwant %s", gj, tc.Expect)
			}
			if errs := h.errs.all(); len(errs) != 0 {
				t.Errorf("errors: %q", errs)
			}
		})
	}
}
