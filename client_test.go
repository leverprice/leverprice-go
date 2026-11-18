package leverprice

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
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
	if !reflect.DeepEqual(body.SDK, map[string]string{"name": "leverprice-go", "version": "0.4.0"}) {
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

func TestManualExposureSendsTheCountry(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.CountryHeader = "CF-IPCountry"
		o.LocalCurrencies = true
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", chrome)
	r.Header.Set("CF-IPCountry", "IN")

	q := h.price(t, "geox", PriceRequest{Request: r, Exposure: ExposureManual})
	if q.Reason != ReasonExperiment || q.Country != "IN" || !q.Regional || q.Currency != "inr" || q.ExposureLogged {
		t.Fatalf("manual quote: %+v", q)
	}
	h.c.Flush(context.Background())
	if events := h.api.sentEvents(t); len(events) != 0 {
		t.Fatalf("events before LogExposure: %v", events)
	}
	if !h.c.LogExposure(q) {
		t.Fatal("LogExposure did not queue")
	}

	unknown := h.price(t, "geox", PriceRequest{UserAgent: chrome, UserID: "user_42", Exposure: ExposureManual})
	if unknown.Country != "" || unknown.Regional || !h.c.LogExposure(unknown) {
		t.Fatalf("manual quote without a country: %+v", unknown)
	}

	h.c.Flush(context.Background())
	events := h.api.sentEvents(t)
	if len(events) != 2 {
		t.Fatalf("%d events: %v", len(events), events)
	}
	if events[0]["type"] != "exposure" || events[0]["visitorId"] != q.VisitorID || events[0]["country"] != "IN" {
		t.Errorf("exposure of the quote from IN: %v", events[0])
	}
	if _, present := events[1]["country"]; present {
		t.Errorf("exposure without a known country: %v", events[1])
	}
}

func TestLogExposureNormalizesTheCountry(t *testing.T) {
	h := newHarness(t)
	cases := []struct{ visitorID, country, sent string }{
		{"v1", " br ", "BR"},
		{"v2", "\u00df", ""},
		{"v3", "usa", ""},
		{"v4", "", ""},
	}
	for _, tc := range cases {
		q := &Quote{Reason: ReasonExperiment, ExperimentID: "exp_1", VariantID: "var_1", VisitorID: tc.visitorID, Country: tc.country}
		if !h.c.LogExposure(q) || !q.ExposureLogged {
			t.Errorf("country %q: exposure not logged", tc.country)
		}
	}
	h.c.Flush(context.Background())
	events := h.api.sentEvents(t)
	if len(events) != len(cases) {
		t.Fatalf("%d events: %v", len(events), events)
	}
	for i, tc := range cases {
		country, present := events[i]["country"]
		if events[i]["visitorId"] != tc.visitorID || present != (tc.sent != "") || (present && country != tc.sent) {
			t.Errorf("country %q: sent %v", tc.country, events[i])
		}
	}
	if errs := h.errs.all(); len(errs) != 0 {
		t.Errorf("errors: %q", errs)
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

func TestCountryHeaderIsValidatedBeforeLowercasing(t *testing.T) {
	for _, name := range []string{"bad name", "x:y", "\u212aey", "\u00a0cf-ipcountry", "cf-ipcountry\u2003"} {
		_, err := New(Options{CountryHeader: name})
		if err == nil || !strings.HasPrefix(err.Error(), "leverprice: ") || !strings.Contains(err.Error(), "CountryHeader") {
			t.Errorf("CountryHeader %q: New returned %v, want an error about CountryHeader", name, err)
		}
	}
	valid := map[string]string{"CF-IPCountry": "cf-ipcountry", " cf-ipcountry\n": "cf-ipcountry", "": "", "  ": ""}
	for name, want := range valid {
		c, err := New(Options{CountryHeader: name})
		if err != nil {
			t.Errorf("CountryHeader %q: %v", name, err)
			continue
		}
		if c.countryHeader != want {
			t.Errorf("CountryHeader %q read as %q, want %q", name, c.countryHeader, want)
		}
	}
}

func TestQuoteJSON(t *testing.T) {
	q := fallbackQuote("pro", guard{currency: "usd", fallbackPrice: 1900}, ReasonNoConfig)
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"productKey":"pro","price":1900,"currency":"usd","country":null,"regional":false,"isFallback":true,` +
		`"reason":"no_config","experimentId":null,"variantId":null,"visitorId":null,"metadata":{},"setCookie":[],` +
		`"checkoutUrl":null,"exposureLogged":false}`
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

func TestCountrySource(t *testing.T) {
	header := http.Header{"Cf-Ipcountry": {"br"}, "X-Country": {"IN", "DE"}, "X-GEO": {"mx"}, "x-\u212aountry": {"IN"}}
	cases := []struct {
		name          string
		countryHeader string
		country       string
		want          string
	}{
		{"no source", "", "", ""},
		{"the header named in the options", "cf-ipcountry", "", "BR"},
		{"a header stored under an uppercase name", "x-geo", "", "MX"},
		{"an explicit country wins", "cf-ipcountry", " de ", "DE"},
		{"an invalid explicit country is not replaced by the header", "cf-ipcountry", "Germany", ""},
		{"a digit is not a letter", "", "1n", ""},
		{"a repeated header is no country", "x-country", "", ""},
		{"a missing header", "x-missing", "", ""},
		{"a header name is matched in ASCII only", "x-kountry", "", ""},
	}
	for _, tc := range cases {
		info := readRequest(&PriceRequest{Header: header, Country: tc.country}, "__pp", tc.countryHeader)
		if info.country != tc.want {
			t.Errorf("%s: country %q, want %q", tc.name, info.country, tc.want)
		}
	}
}

func TestCountryHeaderOptionReplacesTheProjectSetting(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", chrome)
	r.Header.Set("CF-IPCountry", "BR")
	r.Header.Set("X-Geo-Country", "IN")
	cases := []struct {
		name          string
		countryHeader string
		country       string
		want          string
	}{
		{"the project's header", "", "", "IN"},
		{"the option's header", "CF-IPCountry", "", "BR"},
		{"the option's header missing from the request", "X-Country", "", ""},
		{"a country passed in", "CF-IPCountry", "MX", "MX"},
	}
	for _, tc := range cases {
		h := newHarness(t, func(o *Options) { o.CountryHeader = tc.countryHeader })
		q := h.price(t, "geox", PriceRequest{Request: r, Country: tc.country})
		h.c.Flush(context.Background())
		events := h.api.sentEvents(t)
		if q.Reason != ReasonExperiment || q.Country != tc.want || len(events) != 1 {
			t.Errorf("%s: %+v, exposures %v", tc.name, q, events)
			continue
		}
		if country, _ := events[0]["country"].(string); country != tc.want {
			t.Errorf("%s: exposure from %q, want %q", tc.name, country, tc.want)
		}
	}
}

func TestRegionalOptionsAreNormalized(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.CountryHeader = " CF-IPCountry "
		o.LocalCurrencies = true
	})
	q := h.price(t, "geo", PriceRequest{Header: http.Header{"User-Agent": {chrome}, "cf-ipcountry": {" in "}}})
	if q.Reason != ReasonBasePrice || q.Price != 99_900 || q.Currency != "inr" || q.Country != "IN" || !q.Regional ||
		q.IsFallback || q.CheckoutURL != "" {
		t.Fatalf("regional quote: %+v", q)
	}
	if errs := h.errs.unexpected(); len(errs) > 0 {
		t.Errorf("errors: %q", errs)
	}
}

func TestLocalCurrencies(t *testing.T) {
	cases := []struct {
		country  string
		local    bool
		price    int64
		currency string
		regional bool
	}{
		{"IN", false, 2900, "usd", false},
		{"IN", true, 99_900, "inr", true},
		{"BR", false, 2000, "usd", true},
		{"BR", true, 2000, "usd", true},
	}
	for _, tc := range cases {
		h := newHarness(t, func(o *Options) { o.LocalCurrencies = tc.local })
		q := h.price(t, "geo", PriceRequest{UserAgent: chrome, Country: tc.country})
		if q.IsFallback || q.Price != tc.price || q.Currency != tc.currency || q.Regional != tc.regional || q.Country != tc.country {
			t.Errorf("%s with LocalCurrencies %v: %+v", tc.country, tc.local, q)
		}
		if errs := h.errs.unexpected(); len(errs) > 0 {
			t.Errorf("%s with LocalCurrencies %v: errors %q", tc.country, tc.local, errs)
		}
	}
}

func TestMinPriceAndMaxPriceAreIgnored(t *testing.T) {
	inTest := func(experimentID, variantID, country string) PriceRequest {
		state := vectorState{V: returningVisitor.V, A: [][2]string{{experimentID, variantID}}, X: []string{experimentID}}
		req := browser("__pp=" + cookieFor(t, state))
		req.Country = country
		return req
	}
	from := func(country string) PriceRequest { return PriceRequest{UserAgent: chrome, Country: country} }
	quotes := []struct {
		productKey string
		req        PriceRequest
		reason     Reason
		price      int64
		currency   string
		regional   bool
	}{
		{"basic", from(""), ReasonBasePrice, 900, "usd", false},
		{"pro", inTest("exp_A", "var_3", ""), ReasonExperiment, 3900, "usd", false},
		{"pro", PriceRequest{UserAgent: "curl/8.4.0"}, ReasonBot, 2400, "usd", false},
		{"geo", from("BR"), ReasonBasePrice, 2000, "usd", true},
		{"geo", from("IN"), ReasonBasePrice, 99_900, "inr", true},
		{"geox", inTest("exp_G", "var_g1", "IN"), ReasonExperiment, 64_900, "inr", true},
	}
	limits := []struct {
		name               string
		minPrice, maxPrice int64
	}{
		{"narrower than the signed range", 2500, 2600},
		{"MinPrice above MaxPrice", 5000, 100},
		{"negative", -5000, -100},
		{"beyond 2^53-1", math.MinInt64, math.MaxInt64},
	}
	for _, l := range limits {
		h := newHarness(t, func(o *Options) {
			o.LocalCurrencies = true
			for key, g := range o.Products {
				o.Products[key] = ProductGuard{Currency: g.Currency, FallbackPrice: g.FallbackPrice, MinPrice: l.minPrice, MaxPrice: l.maxPrice}
			}
		})
		for _, tc := range quotes {
			q := h.price(t, tc.productKey, tc.req)
			if q.IsFallback || q.Reason != tc.reason || q.Price != tc.price || q.Currency != tc.currency || q.Regional != tc.regional {
				t.Errorf("%s: %s: %+v", l.name, tc.productKey, q)
			}
		}
		if errs := h.errs.unexpected(); len(errs) > 0 {
			t.Errorf("%s: errors %q", l.name, errs)
		}
	}
}

func TestConfigRegions(t *testing.T) {
	const regions = `[{"countries":["BR","MX"],"currency":"usd","price":2000},{"countries":["IN"],"currency":"INR","price":99900}]`
	experiment := func(first, second string) string {
		return `{"id":"exp_1","salt":"s","variants":[{"id":"var_1","price":1900,"weight":5000` + first +
			`},{"id":"var_2","price":3900,"weight":5000` + second + `}]}`
	}
	distinct := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf(`{"countries":["%c%c"],"currency":"inr","price":1}`, 'A'+i/26, 'A'+i%26)
		}
		return "[" + strings.Join(list, ",") + "]"
	}
	products := []struct {
		key   string
		body  string
		valid bool
	}{
		{"flat", `"regions":[]`, true},
		{"none", `"regions":null`, true},
		{"geo", `"regions":` + regions, true},
		{"test", `"regions":` + regions + `,"experiment":` + experiment(`,"regional":[1300,64900]`, `,"regional":[2700,129900]`), true},
		{"ignored", `"experiment":` + experiment(`,"regional":[1]`, `,"regional":"junk"`), true},
		{"limit", `"regions":` + distinct(maxRegions), true},
		{"over", `"regions":` + distinct(maxRegions+1), false},
		{"twice", `"regions":[{"countries":["IN"],"currency":"inr","price":1},{"countries":["BR","IN"],"currency":"usd","price":1}]`, false},
		{"repeated", `"regions":[{"countries":["IN","IN"],"currency":"inr","price":1}]`, false},
		{"lower", `"regions":[{"countries":["in"],"currency":"inr","price":1}]`, false},
		{"empty", `"regions":[{"countries":[],"currency":"inr","price":1}]`, false},
		{"free", `"regions":[{"countries":["IN"],"currency":"inr","price":0}]`, false},
		{"fraction", `"regions":[{"countries":["IN"],"currency":"inr","price":1.5}]`, false},
		{"money", `"regions":[{"countries":["IN"],"currency":"rupee","price":1}]`, false},
		{"shape", `"regions":{"IN":1}`, false},
		{"short", `"regions":` + regions + `,"experiment":` + experiment(`,"regional":[1300]`, `,"regional":[2700,129900]`), false},
		{"long", `"regions":` + regions + `,"experiment":` + experiment(`,"regional":[1300,64900,1]`, `,"regional":[2700,129900]`), false},
		{"missing", `"regions":` + regions + `,"experiment":` + experiment(`,"regional":[1300,64900]`, ``), false},
		{"zero", `"regions":` + regions + `,"experiment":` + experiment(`,"regional":[1300,0]`, `,"regional":[2700,129900]`), false},
	}
	entries := make([]string, len(products))
	var invalid []string
	for i, p := range products {
		entries[i] = fmt.Sprintf(`%q:{"currency":"USD","price":1900,"minPrice":1500,"maxPrice":3900,%s}`, p.key, p.body)
		if !p.valid {
			invalid = append(invalid, p.key)
		}
	}
	payload := `{"schema":2,"projectId":"proj_1","version":1,"expiresAt":1,"keyFingerprints":[],"assignmentKey":"` +
		vectors(t).AssignmentKey + `","products":{` + strings.Join(entries, ",") + `}}`
	cfg, dropped, err := parseConfig(b64url([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dropped, invalid) {
		t.Fatalf("dropped %q, want %q", dropped, invalid)
	}

	if flat := cfg.products["flat"]; len(flat.regions) != 0 || len(flat.regionOf) != 0 {
		t.Errorf("product without regions: %+v", flat)
	}
	geo := cfg.products["geo"]
	if want := []region{{0, "usd", 2000}, {1, "inr", 99_900}}; !slices.Equal(geo.regions, want) {
		t.Errorf("regions %+v, want %+v", geo.regions, want)
	}
	if want := map[string]int{"BR": 0, "MX": 0, "IN": 1}; !reflect.DeepEqual(geo.regionOf, want) {
		t.Errorf("regionOf %v, want %v", geo.regionOf, want)
	}
	for i, want := range [][]int64{{1300, 64_900}, {2700, 129_900}} {
		if got := cfg.products["test"].experiment.variants[i].regional; !slices.Equal(got, want) {
			t.Errorf("variant %d: regional %v, want %v", i, got, want)
		}
	}
	for i, v := range cfg.products["ignored"].experiment.variants {
		if len(v.regional) != 0 {
			t.Errorf("variant %d of a product without regions: regional %v", i, v.regional)
		}
	}
}

func TestConfigCountryHeader(t *testing.T) {
	cases := []struct{ member, want string }{
		{`"countryHeader":"X-Geo-Country",`, "x-geo-country"},
		{`"countryHeader":"cf-ipcountry",`, "cf-ipcountry"},
		{``, ""},
		{`"countryHeader":null,`, ""},
		{`"countryHeader":42,`, ""},
		{`"countryHeader":true,`, ""},
		{`"countryHeader":["x-geo-country"],`, ""},
		{`"countryHeader":{"name":"x-geo-country"},`, ""},
		{`"countryHeader":"",`, ""},
		{`"countryHeader":"cf ipcountry",`, ""},
		{`"countryHeader":" x-geo-country",`, ""},
		{`"countryHeader":"x-geo-country\n",`, ""},
		{`"countryHeader":"x-\u212aountry",`, ""},
	}
	for _, tc := range cases {
		payload := `{"schema":2,"projectId":"proj_1","version":1,"expiresAt":1,"keyFingerprints":[],"assignmentKey":"` +
			vectors(t).AssignmentKey + `",` + tc.member + `"products":{}}`
		cfg, _, err := parseConfig(b64url([]byte(payload)))
		if err != nil {
			t.Errorf("config with %q rejected: %v", tc.member, err)
			continue
		}
		if cfg.countryHeader != tc.want {
			t.Errorf("config with %q: country header %q, want %q", tc.member, cfg.countryHeader, tc.want)
		}
	}
}

func TestConfigTestsByCountry(t *testing.T) {
	test := func(members string) string {
		return `{"id":"exp_1","salt":"s","variants":[{"id":"var_1","price":1900,"weight":5000},` +
			`{"id":"var_2","price":3900,"weight":5000}]` + members + `}`
	}
	open := func(splits string) string { return `"experiment":` + test(`,"splits":`+splits) }
	scoped := func(countries string) string { return `"scopedExperiment":` + test(`,"countries":`+countries) }
	split := func(countries, weights string) string {
		return `{"countries":` + countries + `,"weights":` + weights + `}`
	}
	codes := func(n int) []string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf(`"%c%c"`, 'A'+i/26, 'A'+i%26)
		}
		return list
	}
	countries := func(n int) string { return "[" + strings.Join(codes(n), ",") + "]" }
	splits := func(n int) string {
		list := codes(n)
		for i, code := range list {
			list[i] = split("["+code+"]", "[2500,7500]")
		}
		return "[" + strings.Join(list, ",") + "]"
	}

	valid := map[string]string{
		"open":         `"experiment":` + test(`,"countries":"ignored"`) + `,"scopedExperiment":null`,
		"scoped":       `"experiment":null,` + scoped(`["IN","BR"]`),
		"scope limit":  scoped(countries(maxScope)),
		"split":        open(`[` + split(`["IN","BR"]`, `[10000,0]`) + `,` + split(`["DE"]`, `[2500.0,9007199254740991]`) + `]`),
		"no splits":    open(`null`),
		"empty splits": open(`[]`),
		"splits limit": open(splits(maxSplits)),
		"scoped split": scoped(`["IN"],"splits":[` + split(`["US"]`, `[0,10000]`) + `]`),
	}
	invalid := []string{
		`"experiment":` + test(``) + `,` + scoped(`["IN"]`),
		`"scopedExperiment":` + test(``),
		`"scopedExperiment":[]`,
		`"scopedExperiment":{"id":"exp_1","salt":"s","countries":["IN"],"variants":[{"id":"var_1","price":1900,"weight":-1}]}`,
		scoped(countries(maxScope + 1)),
		scoped(`["IN","BR","IN"]`),
		scoped(`["IN"],"splits":[` + split(`["IN"]`, `[10000]`) + `]`),
		open(splits(maxSplits + 1)),
		open(`{}`),
		open(`["IN"]`),
		open(`[{"weights":[5000,5000]}]`),
		open(`[{"countries":["IN"]}]`),
		open(`[` + split(`["IN"]`, `[5000,5000]`) + `,` + split(`["BR","IN"]`, `[10000,0]`) + `]`),
		open(`[` + split(`["IN","IN"]`, `[5000,5000]`) + `]`),
	}
	for _, list := range []string{`null`, `[]`, `"IN"`, `["in"]`, `["IND"]`, `[1]`, `["\u212aN"]`} {
		invalid = append(invalid, scoped(list), open(`[`+split(list, `[5000,5000]`)+`]`))
	}
	for _, weights := range []string{`null`, `"5000,5000"`, `[10000]`, `[5000,2500,2500]`, `[10001,-1]`, `[0.5,9999.5]`,
		`[9007199254740992,0]`, `[true,0]`, `["5000",5000]`} {
		invalid = append(invalid, open(`[`+split(`["IN"]`, weights)+`]`))
	}

	product := func(key, body string) string {
		return fmt.Sprintf(`%q:{"currency":"usd","price":2900,"minPrice":1900,"maxPrice":4900,%s}`, key, body)
	}
	entries := make([]string, 0, len(valid)+len(invalid))
	for key, body := range valid {
		entries = append(entries, product(key, body))
	}
	for i, body := range invalid {
		entries = append(entries, product(fmt.Sprintf("invalid %d", i), body))
	}
	payload := `{"schema":2,"projectId":"proj_1","version":1,"expiresAt":1,"keyFingerprints":[],"assignmentKey":"` +
		vectors(t).AssignmentKey + `","products":{` + strings.Join(entries, ",") + `}}`
	cfg, _, err := parseConfig(b64url([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	for key := range valid {
		if cfg.products[key] == nil {
			t.Fatalf("dropped the valid product %q", key)
		}
	}
	for i, body := range invalid {
		if cfg.products[fmt.Sprintf("invalid %d", i)] != nil {
			t.Errorf("kept a product with %s", body)
		}
	}

	type set = map[string]struct{}
	type weights = map[string][]float64
	if got := cfg.products["open"].experiment.scope; got != nil {
		t.Errorf("an open test has the scope %v", got)
	}
	if got := cfg.products["scoped"].experiment.scope; !reflect.DeepEqual(got, set{"IN": {}, "BR": {}}) {
		t.Errorf("scope %v, want IN and BR", got)
	}
	if n := len(cfg.products["scope limit"].experiment.scope); n != maxScope {
		t.Errorf("scope of %d countries, want %d", n, maxScope)
	}
	splitOf := cfg.products["split"].experiment.splitOf
	if want := (weights{"IN": {10000, 0}, "BR": {10000, 0}, "DE": {2500, maxSafeInteger}}); !reflect.DeepEqual(splitOf, want) {
		t.Errorf("splits %v, want %v", splitOf, want)
	}
	for _, key := range []string{"open", "no splits", "empty splits"} {
		if n := len(cfg.products[key].experiment.splitOf); n != 0 {
			t.Errorf("%s: %d countries with a split, want none", key, n)
		}
	}
	if n := len(cfg.products["splits limit"].experiment.splitOf); n != maxSplits {
		t.Errorf("%d countries with a split, want %d", n, maxSplits)
	}
	exp := cfg.products["scoped split"].experiment
	if !reflect.DeepEqual(exp.scope, set{"IN": {}}) || !reflect.DeepEqual(exp.splitOf, weights{"US": {0, 10000}}) {
		t.Errorf("scoped test with a split: scope %v, splits %v", exp.scope, exp.splitOf)
	}
}

func TestScopedTestLeavesVisitorsOutsideItsScopeAlone(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Products["local"] = ProductGuard{Currency: "usd", FallbackPrice: 2900} })
	assigned := "__pp=" + cookieFor(t, vectorState{V: returningVisitor.V, A: [][2]string{{"exp_L", "var_l2"}}, X: []string{}})
	for _, country := range []string{"US", "MX", ""} {
		for _, cookie := range []string{"", assigned} {
			req := browser(cookie)
			req.Country = country
			q := h.price(t, "local", req)
			if q.Reason != ReasonBasePrice || q.Country != country || q.VisitorID != "" || len(q.SetCookie) != 0 || q.ExposureLogged {
				t.Errorf("visitor from %q with the cookie %q: %+v", country, cookie, q)
			}
		}
	}
	h.c.Flush(context.Background())
	if events := h.api.sentEvents(t); len(events) != 0 {
		t.Errorf("exposures outside the scope: %v", events)
	}

	req := browser(assigned)
	req.Country = "IN"
	if q := h.price(t, "local", req); q.Reason != ReasonExperiment || q.VariantID != "var_l2" || !q.ExposureLogged {
		t.Errorf("back inside the scope: %+v", q)
	}
}

func TestSplitReplacesTheWeights(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Products["split"] = ProductGuard{Currency: "usd", FallbackPrice: 2900} })
	split := map[string]string{"IN": "var_s3", "BR": "var_s3", "DE": "var_s2", "FR": "var_s1"}
	own := map[string]int{}
	for i := 0; i < 20; i++ {
		req := PriceRequest{UserAgent: chrome, UserID: fmt.Sprintf("user_%d", i)}
		for country, variantID := range split {
			req.Country = country
			if q := h.price(t, "split", req); q.VariantID != variantID {
				t.Errorf("user_%d from %s: %s, want %s", i, country, q.VariantID, variantID)
			}
		}
		for _, country := range []string{"US", ""} {
			req.Country = country
			own[h.price(t, "split", req).VariantID]++
		}
	}
	if len(own) != 2 || own["var_s1"] == 0 || own["var_s2"] == 0 {
		t.Errorf("visitors from a country without a split: %v, want var_s1 and var_s2 only", own)
	}
}

func TestStickyAssignmentBeatsASplit(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Products["split"] = ProductGuard{Currency: "usd", FallbackPrice: 2900} })
	req := browser("__pp=" + cookieFor(t, vectorState{V: returningVisitor.V, A: [][2]string{{"exp_S", "var_s1"}}, X: []string{"exp_S"}}))
	req.Country = "IN"
	if q := h.price(t, "split", req); q.Reason != ReasonExperiment || q.VariantID != "var_s1" || len(q.SetCookie) != 0 {
		t.Errorf("assigned visitor from IN: %+v", q)
	}
	req = browser("")
	req.Country = "IN"
	if q := h.price(t, "split", req); q.VariantID != "var_s3" {
		t.Errorf("new visitor from IN: %s, want var_s3", q.VariantID)
	}
}
