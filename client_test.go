package leverprice

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const chrome = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

var (
	returningVisitor = vectorState{V: "0123456789abcdef0123456789abcdef", A: [][2]string{{"exp_A", "var_3"}}, X: []string{"exp_A"}}
	unexposedVisitor = vectorState{V: "0123456789abcdef0123456789abcdef", A: [][2]string{{"exp_A", "var_2"}}, X: []string{}}
)

func browser(cookie string) PriceRequest {
	return PriceRequest{Header: http.Header{"User-Agent": {chrome}, "Cookie": {cookie}}}
}

func TestColdStartTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	h.c.now = time.Now
	h.api.setConfig(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	start := time.Now()
	var wg sync.WaitGroup
	elapsed := make([]time.Duration, 8)
	for i := range elapsed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 5 * time.Millisecond)
			q := h.price(t, "pro", browser(""))
			if q.Reason != ReasonNoConfig || q.Price != 1900 || !q.IsFallback {
				t.Errorf("quote %d: %s %d", i, q.Reason, q.Price)
			}
			elapsed[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	for i, d := range elapsed {
		if d < 90*time.Millisecond || d > 400*time.Millisecond {
			t.Errorf("quote %d returned after %v, want about 100ms after the fetch started", i, d)
		}
	}
	if n := len(h.api.calls("/v1/config")); n != 1 {
		t.Errorf("%d config fetches, want 1", n)
	}
}

func TestColdStartHonoursContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, func(o *Options) { o.Timeout = 5 * time.Second })
	h.api.setConfig(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	q, err := h.c.GetPrice(ctx, "pro", browser(""))
	if err != nil || q.Reason != ReasonNoConfig {
		t.Fatalf("got %v %v", q, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("GetPrice waited %v despite a 50ms context", d)
	}
}

func TestOutagePinsFallback(t *testing.T) {
	h := newHarness(t)
	h.api.setConfig(status(http.StatusServiceUnavailable, `{"error":{"code":"unavailable"}}`))

	q := h.price(t, "pro", browser(""))
	pin := "__pp_f=1; Path=/; Max-Age=1800; SameSite=Lax; HttpOnly; Secure"
	if q.Reason != ReasonNoConfig || q.Price != 1900 || !reflect.DeepEqual(q.SetCookie, []string{pin}) {
		t.Fatalf("outage quote: %+v", q)
	}
	if errs := h.errs.all(); len(errs) != 1 || errs[0] != "leverprice config: HTTP 503 (unavailable)" {
		t.Errorf("errors = %q", errs)
	}

	h.api.setConfig(newFakeAPI(t).config)
	h.clock.Advance(time.Minute)
	q = h.price(t, "pro", browser("__pp_f=1; __pp="+cookieFor(t, returningVisitor)))
	if q.Reason != ReasonPinnedFallback || q.Price != 1900 || !reflect.DeepEqual(q.SetCookie, []string{pin}) {
		t.Fatalf("pinned quote: %+v", q)
	}
	h.waitFetch()
	q = h.price(t, "pro", browser("__pp="+cookieFor(t, returningVisitor)))
	if q.Reason != ReasonExperiment || q.Price != 3900 {
		t.Fatalf("after the pin: %+v", q)
	}
}

func TestOutagePinsTheRestOfTheRequest(t *testing.T) {
	h := newHarness(t)
	h.api.setConfig(status(http.StatusInternalServerError, ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	r.Header.Set("User-Agent", chrome)

	if q := h.price(t, "pro", PriceRequest{Request: r}); q.Reason != ReasonNoConfig {
		t.Fatalf("first quote: %s", q.Reason)
	}
	h.api.setConfig(newFakeAPI(t).config)
	h.c.Preload(context.Background())
	if q := h.price(t, "basic", PriceRequest{Request: r}); q.Reason != ReasonPinnedFallback {
		t.Fatalf("second quote of the same request: %s", q.Reason)
	}
}

func TestBackoffAfterFailure(t *testing.T) {
	h := newHarness(t)
	h.api.setConfig(status(http.StatusBadGateway, ""))
	h.price(t, "pro", browser(""))
	h.price(t, "pro", browser(""))
	if n := len(h.api.calls("/v1/config")); n != 1 {
		t.Fatalf("%d fetches while backing off, want 1", n)
	}
	h.clock.Advance(1001 * time.Millisecond)
	h.price(t, "pro", browser(""))
	if n := len(h.api.calls("/v1/config")); n != 2 {
		t.Fatalf("%d fetches after the backoff, want 2", n)
	}
}

func TestPreloadResetsBackoff(t *testing.T) {
	h := newHarness(t)
	h.api.setConfig(status(http.StatusBadGateway, ""))
	h.c.Preload(context.Background())
	h.api.setConfig(newFakeAPI(t).config)
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonBasePrice {
		t.Fatalf("first request after a failed preload: %s", q.Reason)
	}
}

func TestRevalidationKeepsConfig(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxStale = time.Minute })
	var mu sync.Mutex
	var conditional []string
	full := newFakeAPI(t).config
	h.api.setConfig(func(w http.ResponseWriter, r *http.Request) {
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			mu.Lock()
			conditional = append(conditional, inm)
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full(w, r)
	})
	h.c.Preload(context.Background())

	h.clock.Advance(40 * time.Second)
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonBasePrice {
		t.Fatalf("before revalidation: %s", q.Reason)
	}
	h.waitFetch()
	mu.Lock()
	sent := append([]string(nil), conditional...)
	mu.Unlock()
	if !reflect.DeepEqual(sent, []string{`"v5"`}) {
		t.Fatalf("If-None-Match sent: %q", sent)
	}
	h.clock.Advance(30 * time.Second)
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonBasePrice {
		t.Fatalf("after 304, past the original MaxStale: %s", q.Reason)
	}
	if errs := h.errs.unexpected(); len(errs) > 0 {
		t.Errorf("errors: %q", errs)
	}
}

func TestStaleConfigIsNotUsed(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxStale = time.Minute; o.Timeout = -1 })
	h.c.Preload(context.Background())
	h.api.setConfig(status(http.StatusServiceUnavailable, ""))
	h.clock.Advance(61 * time.Second)
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonNoConfig {
		t.Fatalf("stale config served: %s", q.Reason)
	}
}

func TestNewerConfigVersionTriggersUnconditionalFetch(t *testing.T) {
	h := newHarness(t)
	h.c.Preload(context.Background())
	h.api.setEvents(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Leverprice-Config-Version", "6")
		w.WriteHeader(http.StatusAccepted)
	})
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 2900, Currency: "USD"})
	h.waitFetch()
	calls := h.api.calls("/v1/config")
	if len(calls) != 2 {
		t.Fatalf("%d config fetches, want 2", len(calls))
	}
	if inm := calls[1].header.Get("If-None-Match"); inm != "" {
		t.Errorf("catch-up fetch sent If-None-Match %q", inm)
	}
	h.price(t, "basic", browser(""))
	if n := len(h.api.calls("/v1/config")); n != 2 {
		t.Errorf("catch-up repeated within 5s: %d fetches", n)
	}
}

func TestFailedSendRequeuesPurchasesAndDropsExposures(t *testing.T) {
	h := newHarness(t)
	h.api.setEvents(status(http.StatusServiceUnavailable, "down"))
	q := h.price(t, "pro", browser("__pp="+cookieFor(t, unexposedVisitor)))
	if !q.ExposureLogged {
		t.Fatal("exposure not logged")
	}
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 2900, Currency: "usd", Metadata: q.Metadata})

	if n := len(h.api.calls("/v1/events")); n != sendAttempts {
		t.Fatalf("%d attempts, want %d", n, sendAttempts)
	}
	h.c.qmu.Lock()
	queued := append([]wireEvent(nil), h.c.queue...)
	h.c.qmu.Unlock()
	if len(queued) != 1 || queued[0].eventType() != "purchase" {
		t.Fatalf("queue after failure: %+v", queued)
	}
	if errs := h.errs.unexpected(); len(errs) != 1 || errs[0] != "leverprice events: HTTP 503" {
		t.Errorf("errors = %q", errs)
	}

	h.api.setEvents(newFakeAPI(t).events)
	h.c.Flush(context.Background())
	calls := h.api.calls("/v1/events")
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(calls[len(calls)-1].body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0]["type"] != "purchase" {
		t.Fatalf("retried batch: %v", body.Events)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.api.setEvents(status(http.StatusBadRequest, `{"error":"bad"}`))
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 2900, Currency: "usd"})
	if n := len(h.api.calls("/v1/events")); n != 1 {
		t.Fatalf("%d attempts, want 1", n)
	}
	if errs := h.errs.all(); len(errs) != 1 || errs[0] != `leverprice events rejected: HTTP 400 {"error":"bad"}` {
		t.Errorf("errors = %q", errs)
	}
	h.c.qmu.Lock()
	defer h.c.qmu.Unlock()
	if len(h.c.queue) != 0 {
		t.Errorf("rejected batch kept: %d events", len(h.c.queue))
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	attempts := 0
	h.api.setEvents(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 2900, Currency: "usd"})
	if n := len(h.api.calls("/v1/events")); n != 2 {
		t.Fatalf("%d attempts, want 2", n)
	}
	if errs := h.errs.all(); len(errs) != 0 {
		t.Errorf("errors = %q", errs)
	}
}

func TestKillSwitchOption(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Disabled = true })
	q := h.price(t, "pro", browser("__pp="+cookieFor(t, returningVisitor)))
	if q.Reason != ReasonDisabled || q.Price != 1900 || len(q.SetCookie) != 0 {
		t.Fatalf("disabled quote: %+v", q)
	}
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 1, Currency: "usd"})
	h.c.Refresh(context.Background())
	h.c.Preload(context.Background())
	if h.c.LogExposure(&Quote{Reason: ReasonExperiment, ExperimentID: "e", VariantID: "v", VisitorID: "x"}) {
		t.Error("LogExposure queued while disabled")
	}
	if n := len(h.api.requests); n != 0 {
		t.Errorf("%d requests while disabled", n)
	}
	if errs := h.errs.all(); len(errs) != 0 {
		t.Errorf("errors while disabled: %q", errs)
	}
}

func TestKillSwitchEnvironment(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LEVERPRICE_DISABLED", " True ")
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonDisabled {
		t.Fatalf("with LEVERPRICE_DISABLED: %s", q.Reason)
	}
	if n := len(h.api.requests); n != 0 {
		t.Errorf("%d requests while disabled", n)
	}
	t.Setenv("LEVERPRICE_DISABLED", "0")
	if q := h.price(t, "basic", browser("")); q.Reason != ReasonBasePrice {
		t.Fatalf("after clearing LEVERPRICE_DISABLED: %s", q.Reason)
	}
}

func TestMalformedSecretKey(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.SecretKey = "  pp_sk_live_tooShort  " })
	if errs := h.errs.all(); len(errs) != 1 || !strings.Contains(errs[0], "SecretKey") {
		t.Fatalf("errors = %q", errs)
	}
	if q := h.price(t, "pro", browser("")); q.Reason != ReasonDisabled {
		t.Fatalf("quote: %s", q.Reason)
	}
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 1, Currency: "usd"})
	if len(h.api.requests) != 0 || len(h.errs.all()) != 1 {
		t.Errorf("requests %d, errors %q", len(h.api.requests), h.errs.all())
	}

	quiet := newHarness(t, func(o *Options) { o.SecretKey = "" })
	disabled := newHarness(t, func(o *Options) { o.SecretKey = "nope"; o.Disabled = true })
	if len(quiet.errs.all())+len(disabled.errs.all()) != 0 {
		t.Error("missing or disabled key reported an error")
	}
}

func TestPanickingOnErrorChangesNothing(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.SecretKey = "pp_sk_live_bad"
		o.OnError = func(error) { panic("logger exploded") }
	})
	if q := h.price(t, "pro", browser("")); q.Reason != ReasonDisabled {
		t.Fatalf("quote: %s", q.Reason)
	}

	h = newHarness(t, func(o *Options) { o.OnError = func(error) { panic("logger exploded") } })
	h.api.setConfig(status(http.StatusInternalServerError, ""))
	h.api.setEvents(status(http.StatusInternalServerError, ""))
	if q := h.price(t, "pro", browser("")); q.Reason != ReasonNoConfig || q.Price != 1900 {
		t.Fatalf("quote: %s %d", q.Reason, q.Price)
	}
	h.c.TrackPurchase(context.Background(), Purchase{Provider: ProviderStripe, OrderID: "pi_1", Amount: 1, Currency: "usd"})
	h.c.TrackPurchase(context.Background(), Purchase{Provider: "paypal", OrderID: "x", Amount: 1, Currency: "usd"})
}

func TestTrackPurchase(t *testing.T) {
	h := newHarness(t)
	q := h.price(t, "pro", browser("__pp="+cookieFor(t, returningVisitor)))
	h.c.TrackPurchase(context.Background(), Purchase{
		Provider:  ProviderStripe,
		OrderID:   "pi_123",
		Amount:    3900,
		Currency:  "USD",
		Metadata:  q.Metadata,
		Timestamp: 1_800_000_000,
	})
	h.c.TrackPurchase(context.Background(), Purchase{
		Provider: ProviderPolar,
		OrderID:  "ord_9",
		Amount:   100,
		Currency: "eur",
		Metadata: map[string]string{"pp_exp": "exp_A", "pp_var": "var_3", "pp_vid": q.VisitorID, "pp_sig": "NOT-HEX"},
	})
	h.c.TrackPurchase(context.Background(), Purchase{
		Provider: ProviderOther,
		OrderID:  "ord_10",
		Amount:   100,
		Currency: "eur",
		Metadata: map[string]string{"pp_exp": "exp_A", "pp_var": "bad id!", "pp_vid": q.VisitorID},
	})
	events := h.api.sentEvents(t)
	if len(events) != 3 {
		t.Fatalf("%d events sent: %v", len(events), events)
	}
	want := map[string]any{
		"type": "purchase", "provider": "stripe", "orderId": "pi_123", "amount": 3900.0, "currency": "usd",
		"timestamp": 1_800_000_000_000.0,
		"attribution": map[string]any{
			"experimentId": "exp_A", "variantId": "var_3", "visitorId": q.VisitorID, "signature": q.Metadata["pp_sig"],
		},
	}
	if !reflect.DeepEqual(events[0], want) {
		t.Errorf("purchase\n got %v\nwant %v", events[0], want)
	}
	if a := events[1]["attribution"].(map[string]any); a["signature"] != nil {
		t.Errorf("invalid pp_sig kept: %v", a)
	}
	if events[1]["timestamp"] != float64(vectors(t).Now) {
		t.Errorf("default timestamp = %v", events[1]["timestamp"])
	}
	if events[2]["attribution"] != nil {
		t.Errorf("attribution with an invalid id: %v", events[2]["attribution"])
	}
}

func TestTrackRefund(t *testing.T) {
	h := newHarness(t)
	total := int64(2900)
	h.c.TrackRefund(context.Background(), Refund{Provider: ProviderStripe, OrderID: "pi_1", Currency: "usd", RefundID: "re_1", Amount: 500})
	h.c.TrackRefund(context.Background(), Refund{Provider: ProviderOther, OrderID: "pi_1", Currency: "usd", RefundedTotal: &total})
	h.c.TrackRefund(context.Background(), Refund{Provider: ProviderStripe, OrderID: "pi_1", Currency: "usd"})
	events := h.api.sentEvents(t)
	if len(events) != 2 {
		t.Fatalf("%d events: %v", len(events), events)
	}
	if events[0]["refundId"] != "re_1" || events[0]["amount"] != 500.0 || events[0]["refundedTotal"] != nil {
		t.Errorf("refund by id: %v", events[0])
	}
	if events[1]["refundedTotal"] != 2900.0 || events[1]["refundId"] != nil || events[1]["amount"] != nil {
		t.Errorf("cumulative refund: %v", events[1])
	}
	if errs := h.errs.all(); len(errs) != 1 || !strings.HasPrefix(errs[0], "leverprice: dropped invalid refund event") {
		t.Errorf("errors = %q", errs)
	}
}

func TestRequestHeaders(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.APIURL = "https://api.example.test//" })
	q := h.price(t, "pro", browser("__pp="+cookieFor(t, unexposedVisitor)))
	h.c.Flush(context.Background())
	if len(h.api.requests) != 2 {
		t.Fatalf("%d requests", len(h.api.requests))
	}
	for _, r := range h.api.requests {
		if got := r.header.Get("Authorization"); got != "Bearer "+vectors(t).SecretKey {
			t.Errorf("%s: Authorization %q", r.path, got)
		}
		if got := r.header.Get("User-Agent"); got != "leverprice-sdk-go/"+Version {
			t.Errorf("%s: User-Agent %q", r.path, got)
		}
		if _, ok := r.header["Origin"]; ok {
			t.Errorf("%s: sent an Origin header", r.path)
		}
	}
	events := h.api.calls("/v1/events")[0]
	if ct := events.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	var body struct {
		SDK map[string]string `json:"sdk"`
	}
	_ = json.Unmarshal(events.body, &body)
	if !reflect.DeepEqual(body.SDK, map[string]string{"name": "leverprice-go", "version": "0.3.0"}) {
		t.Errorf("sdk = %v", body.SDK)
	}
	if !q.ExposureLogged {
		t.Error("exposure not logged")
	}
}

func TestQuotesOfOneRequestShareTheCookie(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	r.Header.Set("User-Agent", chrome)

	first := h.price(t, "pro", PriceRequest{Request: r})
	second := h.price(t, "pro", PriceRequest{Request: r})
	if first.Reason != ReasonExperiment || second.VisitorID != first.VisitorID || second.VariantID != first.VariantID {
		t.Fatalf("quotes differ: %+v / %+v", first, second)
	}
	if len(first.SetCookie) != 1 || len(second.SetCookie) != 0 {
		t.Errorf("cookies: %v / %v", first.SetCookie, second.SetCookie)
	}
	other := h.price(t, "pro", PriceRequest{Header: r.Header})
	if other.VisitorID == first.VisitorID {
		t.Error("a request without identity shared the visitor")
	}

	cancel()
	eventually(t, "scope cleanup", func() bool {
		h.c.scopeMu.Lock()
		defer h.c.scopeMu.Unlock()
		return len(h.c.scopes) == 0
	})
}

func TestManualExposure(t *testing.T) {
	h := newHarness(t)
	q := h.price(t, "pro", PriceRequest{Header: browser("__pp=" + cookieFor(t, unexposedVisitor)).Header, Exposure: ExposureManual})
	if q.ExposureLogged || len(q.SetCookie) != 0 {
		t.Fatalf("manual quote: %+v", q)
	}
	if !h.c.LogExposure(q) || !q.ExposureLogged {
		t.Fatal("LogExposure did not queue")
	}
	if h.c.LogExposure(q) {
		t.Error("LogExposure queued twice")
	}
	h.c.Flush(context.Background())
	if events := h.api.sentEvents(t); len(events) != 1 || events[0]["variantId"] != "var_2" {
		t.Errorf("events = %v", events)
	}
}

func TestExposureTimer(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.FlushInterval = 20 * time.Millisecond })
	h.price(t, "pro", browser("__pp="+cookieFor(t, unexposedVisitor)))
	eventually(t, "timed flush", func() bool { return len(h.api.calls("/v1/events")) == 1 })
}

func TestCloseFlushesAndLeavesNothingRunning(t *testing.T) {
	before := runtime.NumGoroutine()
	h := newHarness(t, func(o *Options) { o.FlushInterval = time.Hour })
	h.price(t, "pro", browser("__pp="+cookieFor(t, unexposedVisitor)))
	h.c.Close(context.Background())
	if n := len(h.api.calls("/v1/events")); n != 1 {
		t.Fatalf("Close sent %d batches, want 1", n)
	}
	h.c.qmu.Lock()
	timer := h.c.timer
	h.c.qmu.Unlock()
	if timer != nil {
		t.Error("flush timer still armed after Close")
	}
	if q := h.price(t, "pro", browser("__pp="+cookieFor(t, returningVisitor))); q.Reason != ReasonExperiment {
		t.Errorf("after Close: %s", q.Reason)
	}
	h.clock.Advance(time.Hour)
	h.price(t, "pro", browser(""))
	if n := len(h.api.calls("/v1/config")); n != 1 {
		t.Errorf("fetched the config %d times after Close", n)
	}
	eventually(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}

func TestBots(t *testing.T) {
	h := newHarness(t)
	q := h.price(t, "pro", PriceRequest{UserAgent: "curl/8.4.0"})
	if q.Reason != ReasonBot || q.Price != 2400 || q.IsFallback || q.CheckoutURL != "https://buy.example.com/pro" {
		t.Fatalf("bot quote: %+v", q)
	}
	cold := newHarness(t)
	cold.api.setConfig(status(http.StatusServiceUnavailable, ""))
	q = cold.price(t, "pro", PriceRequest{UserAgent: "Googlebot/2.1"})
	if q.Reason != ReasonBot || q.Price != 1900 || !q.IsFallback || len(q.SetCookie) != 0 {
		t.Fatalf("bot quote without a config: %+v", q)
	}
}

func TestLegacyCookieValue(t *testing.T) {
	h := newHarness(t)
	q := h.price(t, "pro", PriceRequest{UserAgent: chrome, Cookie: " " + cookieFor(t, returningVisitor) + " "})
	if q.Reason != ReasonExperiment || q.VariantID != "var_3" || len(q.SetCookie) != 0 {
		t.Fatalf("legacy cookie: %+v", q)
	}
}

func TestOptionsValidation(t *testing.T) {
	cases := map[string]Options{
		"currency":      {Products: map[string]ProductGuard{"p": {Currency: "us", FallbackPrice: 1}}},
		"fallback":      {Products: map[string]ProductGuard{"p": {Currency: "usd"}}},
		"bounds":        {Products: map[string]ProductGuard{"p": {Currency: "usd", FallbackPrice: 1, MinPrice: 10, MaxPrice: 5}}},
		"unsafe max":    {Products: map[string]ProductGuard{"p": {Currency: "usd", FallbackPrice: 1, MaxPrice: 1 << 60}}},
		"cookie name":   {CookieName: "bad name"},
		"cookie name 2": {CookieName: "a;b"},
	}
	for name, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New accepted invalid options", name)
		}
	}
	c, err := New(Options{Products: map[string]ProductGuard{"p": {Currency: "USD", FallbackPrice: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if q, err := c.GetPrice(context.Background(), "p", PriceRequest{}); err != nil || q.Currency != "usd" {
		t.Errorf("quote %+v, %v", q, err)
	}
	if _, err := c.GetPrice(context.Background(), "nope", PriceRequest{}); !errors.Is(err, ErrUnknownProduct) {
		t.Errorf("unknown product error = %v", err)
	}
}

func TestQuoteJSON(t *testing.T) {
	q := fallbackQuote("pro", guard{currency: "usd", fallbackPrice: 1900}, ReasonNoConfig)
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"productKey":"pro","price":1900,"currency":"usd","isFallback":true,"reason":"no_config",` +
		`"experimentId":null,"variantId":null,"visitorId":null,"metadata":{},"setCookie":[],"checkoutUrl":null,"exposureLogged":false}`
	if string(b) != want {
		t.Errorf("JSON\n got %s\nwant %s", b, want)
	}
}

func TestWriteCookies(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Quote{SetCookie: []string{"a=1", "b=2"}}).WriteCookies(rec)
	if got := rec.Header().Values("Set-Cookie"); !reflect.DeepEqual(got, []string{"a=1", "b=2"}) {
		t.Errorf("Set-Cookie = %q", got)
	}
	var nilQuote *Quote
	nilQuote.WriteCookies(rec)
}

func TestQueueBound(t *testing.T) {
	h := newHarness(t)
	h.c.qmu.Lock()
	h.c.queue = append(h.c.queue, purchaseEvent{Type: "purchase", OrderID: "first"})
	for i := 0; i < maxQueue-1; i++ {
		h.c.queue = append(h.c.queue, exposureEvent{Type: "exposure"})
	}
	h.c.queue = append(h.c.queue, purchaseEvent{Type: "purchase", OrderID: "second"})
	dropped := h.c.trimQueueLocked()
	first := h.c.queue[0]
	h.c.qmu.Unlock()
	if len(dropped) != 0 || first.(purchaseEvent).OrderID != "first" {
		t.Errorf("an exposure should go first: dropped %v", dropped)
	}
}

func TestRootPublicKeys(t *testing.T) {
	keys := RootPublicKeys()
	if !reflect.DeepEqual(keys, map[string]string{"r1": "5FmNzXvPMPZEuDnFT_fFd_VVNsUrWwWTKLerMWbSdcg"}) {
		t.Fatalf("RootPublicKeys = %v", keys)
	}
	for id, k := range keys {
		raw, err := fromB64url(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Errorf("root key %s is not a 32-byte base64url key", id)
		}
	}
	keys["r2"] = "x"
	if len(RootPublicKeys()) != 1 {
		t.Error("RootPublicKeys returned a shared map")
	}
}

func TestHeaderValue(t *testing.T) {
	h := http.Header{"Cookie": {"a=1", "b=2"}, "x-purpose": {"preview"}, "Accept": {"x", "y"}}
	if v, _ := headerValue(h, "cookie"); v != "a=1; b=2" {
		t.Errorf("cookie = %q", v)
	}
	if v, _ := headerValue(h, "accept"); v != "x, y" {
		t.Errorf("accept = %q", v)
	}
	if v, ok := headerValue(h, "X-Purpose"); !ok || v != "preview" {
		t.Errorf("non-canonical key = %q, %v", v, ok)
	}
	if _, ok := headerValue(nil, "cookie"); ok {
		t.Error("nil header has a value")
	}
}
