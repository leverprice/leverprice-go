package leverprice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type vectorState struct {
	V string      `json:"v"`
	A [][2]string `json:"a"`
	X []string    `json:"x"`
}

func (s *vectorState) state() *assignmentState {
	st := newAssignmentState(s.V)
	for _, p := range s.A {
		st.a = append(st.a, assignmentPair{p[0], p[1]})
	}
	st.x = append(st.x, s.X...)
	return st
}

type vectorGuard struct {
	Currency      string `json:"currency"`
	FallbackPrice int64  `json:"fallbackPrice"`
	MinPrice      int64  `json:"minPrice"`
	MaxPrice      int64  `json:"maxPrice"`
}

type vectorSet struct {
	Now             int64             `json:"now"`
	RootKeys        map[string]string `json:"rootKeys"`
	SecretKey       string            `json:"secretKey"`
	OtherSecretKey  string            `json:"otherSecretKey"`
	AssignmentKey   string            `json:"assignmentKey"`
	Config          json.RawMessage   `json:"config"`
	KeyFingerprints []struct {
		SecretKey   string `json:"secretKey"`
		Fingerprint string `json:"fingerprint"`
	} `json:"keyFingerprints"`
	Trust []struct {
		Name             string `json:"name"`
		MinKeysetVersion *int64 `json:"minKeysetVersion"`
		Steps            []struct {
			Envelope json.RawMessage `json:"envelope"`
			Now      *int64          `json:"now"`
			Expect   string          `json:"expect"`
			Version  int64           `json:"version"`
			Dropped  []string        `json:"dropped"`
		} `json:"steps"`
	} `json:"trust"`
	VisitorIDs []struct {
		VisitorKey string `json:"visitorKey"`
		Message    string `json:"message"`
		VisitorID  string `json:"visitorId"`
	} `json:"visitorIds"`
	Attribution []struct {
		ExperimentID string `json:"experimentId"`
		VariantID    string `json:"variantId"`
		VisitorID    string `json:"visitorId"`
		Message      string `json:"message"`
		Signature    string `json:"signature"`
	} `json:"attribution"`
	Buckets []struct {
		Salt      string `json:"salt"`
		VisitorID string `json:"visitorId"`
		Bucket    int    `json:"bucket"`
	} `json:"buckets"`
	PickVariant []struct {
		Weights []float64 `json:"weights"`
		Bucket  int       `json:"bucket"`
		Index   int       `json:"index"`
	} `json:"pickVariant"`
	Cookies struct {
		Sign []struct {
			State  vectorState `json:"state"`
			JSON   string      `json:"json"`
			Cookie string      `json:"cookie"`
		} `json:"sign"`
		Verify []struct {
			Name   string       `json:"name"`
			Cookie string       `json:"cookie"`
			State  *vectorState `json:"state"`
		} `json:"verify"`
		Ops []struct {
			Name  string      `json:"name"`
			State vectorState `json:"state"`
			Op    struct {
				Assign      *[2]string `json:"assign"`
				MarkExposed *string    `json:"markExposed"`
			} `json:"op"`
			Changed *bool       `json:"changed"`
			Result  vectorState `json:"result"`
		} `json:"ops"`
	} `json:"cookies"`
	CookieHeader []struct {
		Header string   `json:"header"`
		Name   string   `json:"name"`
		Values []string `json:"values"`
	} `json:"cookieHeader"`
	Prefetch []struct {
		Headers  map[string]string `json:"headers"`
		Prefetch bool              `json:"prefetch"`
	} `json:"prefetch"`
	SetCookie []struct {
		Name   string  `json:"name"`
		Value  string  `json:"value"`
		MaxAge int64   `json:"maxAge"`
		Secure bool    `json:"secure"`
		Domain *string `json:"domain"`
		Header string  `json:"header"`
	} `json:"setCookie"`
	Bots []struct {
		UserAgent string `json:"userAgent"`
		Bot       bool   `json:"bot"`
	} `json:"bots"`
	Events []struct {
		Event json.RawMessage `json:"event"`
		Valid bool            `json:"valid"`
	} `json:"events"`
	Quotes []struct {
		Name       string                 `json:"name"`
		ProductKey string                 `json:"productKey"`
		Guards     map[string]vectorGuard `json:"guards"`
		Headers    map[string]string      `json:"headers"`
		UserID     *string                `json:"userId"`
		Exposure   string                 `json:"exposure"`
		Bot        *bool                  `json:"bot"`
		Expect     json.RawMessage        `json:"expect"`
	} `json:"quotes"`
}

var (
	vectorsOnce sync.Once
	vectorsData *vectorSet
	vectorsErr  error
)

func vectors(t testing.TB) *vectorSet {
	t.Helper()
	vectorsOnce.Do(func() {
		path, err := findVectors()
		if err != nil {
			vectorsErr = err
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			vectorsErr = err
			return
		}
		vectorsData = &vectorSet{}
		vectorsErr = json.Unmarshal(raw, vectorsData)
	})
	if vectorsErr != nil {
		t.Fatalf("conformance vectors: %v", vectorsErr)
	}
	return vectorsData
}

func findVectors() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, "conformance", "vectors.json")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("conformance/vectors.json not found in any parent directory")
		}
		dir = parent
	}
}

func defaultGuards() map[string]ProductGuard {
	return map[string]ProductGuard{
		"pro":   {Currency: "usd", FallbackPrice: 1900, MinPrice: 1500, MaxPrice: 3900},
		"basic": {Currency: "usd", FallbackPrice: 1000},
		"euro":  {Currency: "eur", FallbackPrice: 1200, MinPrice: 100, MaxPrice: 5000},
		"wild":  {Currency: "usd", FallbackPrice: 2000, MinPrice: 1000, MaxPrice: 3000},
		"bad":   {Currency: "usd", FallbackPrice: 150},
		"ghost": {Currency: "usd", FallbackPrice: 4900},
	}
}

type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

type fakeAPI struct {
	mu       sync.Mutex
	config   http.HandlerFunc
	events   http.HandlerFunc
	requests []recordedRequest
}

func newFakeAPI(t testing.TB) *fakeAPI {
	v := vectors(t)
	return &fakeAPI{
		config: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v5"`)
			_, _ = w.Write(v.Config)
		},
		events: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"accepted":1,"dropped":0}`)
		},
	}
}

func (f *fakeAPI) Do(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path, r.Header.Clone(), body})
	handler := f.events
	if r.URL.Path == "/v1/config" {
		handler = f.config
	}
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	handler(rec, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return rec.Result(), nil
}

func (f *fakeAPI) setConfig(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = h
}

func (f *fakeAPI) setEvents(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = h
}

func (f *fakeAPI) calls(path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) sentEvents(t testing.TB) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range f.calls("/v1/events") {
		var body struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil {
			t.Fatalf("events body: %v", err)
		}
		out = append(out, body.Events...)
	}
	return out
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type errorLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *errorLog) add(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, err.Error())
}

func (l *errorLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

func (l *errorLog) unexpected() []string {
	var out []string
	for _, m := range l.all() {
		if !strings.Contains(m, "malformed products") {
			out = append(out, m)
		}
	}
	return out
}

type harness struct {
	c     *Client
	api   *fakeAPI
	clock *fakeClock
	errs  *errorLog
}

func newHarness(t testing.TB, configure ...func(*Options)) *harness {
	t.Helper()
	v := vectors(t)
	h := &harness{api: newFakeAPI(t), clock: &fakeClock{t: time.UnixMilli(v.Now)}, errs: &errorLog{}}
	opts := Options{
		SecretKey:       v.SecretKey,
		Products:        defaultGuards(),
		TrustedRootKeys: v.RootKeys,
		HTTPClient:      h.api,
		OnError:         h.errs.add,
		FlushInterval:   time.Hour,
	}
	for _, f := range configure {
		f(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.now = h.clock.Now
	c.sleep = func(time.Duration) {}
	h.c = c
	t.Cleanup(func() { c.Close(context.Background()) })
	return h
}

func (h *harness) price(t testing.TB, key string, req PriceRequest) *Quote {
	t.Helper()
	q, err := h.c.GetPrice(context.Background(), key, req)
	if err != nil {
		t.Fatalf("GetPrice(%q): %v", key, err)
	}
	return q
}

func (h *harness) waitFetch() {
	h.c.mu.Lock()
	f := h.c.flight
	h.c.mu.Unlock()
	if f != nil {
		<-f.done
	}
}

func headerOf(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cookieFor(t testing.TB, s vectorState) string {
	t.Helper()
	key, err := fromB64url(vectors(t).AssignmentKey)
	if err != nil {
		t.Fatal(err)
	}
	return signAssignment(key, s.state())
}
