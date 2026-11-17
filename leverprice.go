package leverprice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the SDK version.
const Version = "0.3.0"

// MinKeysetVersion is the lowest keyset version a new Client accepts.
const MinKeysetVersion = 1

// ErrUnknownProduct is returned by GetPrice for a product key that is not in Options.Products.
var ErrUnknownProduct = errors.New("leverprice: unknown product")

// RootPublicKeys returns a copy of Leverprice's production root public keys (raw Ed25519,
// base64url) by key id.
func RootPublicKeys() map[string]string {
	return map[string]string{"r1": "5FmNzXvPMPZEuDnFT_fFd_VVNsUrWwWTKLerMWbSdcg"}
}

const (
	defaultAPIURL          = "https://api.leverprice.com"
	defaultTimeout         = 500 * time.Millisecond
	defaultRefreshInterval = 30 * time.Second
	defaultMaxStale        = 24 * time.Hour
	defaultCookieName      = "__pp"
	defaultCookieMaxAge    = 180 * 24 * time.Hour
	defaultFlushInterval   = 2 * time.Second

	requestTimeout = 10 * time.Second
	stallMs        = int64(requestTimeout/time.Millisecond) + 5000
	maxBackoffMs   = 30_000
	catchUpGapMs   = 5_000
	maxQueue       = 5_000
	autoFlushAt    = 100
	sendAttempts   = 3
	maxDedupe      = 10_000
	maxScopes      = 100_000
	maxBodyLength  = 16 << 20
)

// HTTPDoer sends HTTP requests; *http.Client implements it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ProductGuard holds the local guardrails for one product. Prices are in minor units.
type ProductGuard struct {
	Currency      string // ISO 4217 code, e.g. "usd"
	FallbackPrice int64  // served whenever Leverprice cannot price the product; must be positive
	MinPrice      int64  // lowest price ever returned; 0 means 1
	MaxPrice      int64  // highest price ever returned; 0 means 2^53-1
}

// Options configure a Client. Zero values select the defaults.
type Options struct {
	SecretKey       string                  // pp_sk_live_… or pp_sk_test_…; missing or malformed disables the SDK
	Products        map[string]ProductGuard // guardrails by product key
	Disabled        bool                    // kill switch; LEVERPRICE_DISABLED=1|true does the same
	APIURL          string                  // default https://api.leverprice.com
	Timeout         time.Duration           // cold-start wait for a config; default 500ms, negative for none
	RefreshInterval time.Duration           // config revalidation period; default 30s
	MaxStale        time.Duration           // stop using a config not confirmed for this long; default 24h
	CookieName      string                  // assignment cookie name; default "__pp" (the pin is CookieName+"_f")
	CookieMaxAge    time.Duration           // assignment cookie lifetime; default 180 days
	CookieSecure    *bool                   // Secure cookie attribute; default true
	CookieDomain    string                  // Domain cookie attribute; default none
	FlushInterval   time.Duration           // longest time exposures wait before being sent; default 2s
	TrustedRootKeys map[string]string       // root public keys by id; default RootPublicKeys()
	HTTPClient      HTTPDoer                // default a plain *http.Client
	OnError         func(error)             // receives every SDK error; default a slog warning
}

// ExposureMode selects when an experiment exposure is logged.
type ExposureMode uint8

// Exposure modes.
const (
	ExposureAuto   ExposureMode = iota // log when the price is computed (not for prefetches)
	ExposureManual                     // log with Client.LogExposure once the price is visible
)

// PriceRequest describes the visitor a price is for. Every field is optional.
type PriceRequest struct {
	Request   *http.Request // incoming request; quotes for the same request share one cookie
	Header    http.Header   // alternative to Request; wins over Request.Header
	Cookie    string        // overrides the Cookie header; a value without "=" is the bare cookie value
	UserAgent string        // overrides the User-Agent header
	UserID    string        // stable signed-in user id; only its HMAC leaves the server
	Exposure  ExposureMode
	Bot       *bool // own bot verdict; skips the built-in user-agent check
}

type guard struct {
	currency      string
	fallbackPrice int64
	minPrice      int64
	maxPrice      int64
}

// Client prices products and reports events. It is safe for concurrent use; create one per
// process and share it.
type Client struct {
	secretKey     string
	fingerprint   string
	guards        map[string]guard
	disabled      bool
	apiURL        string
	timeoutMs     int64
	refreshMs     int64
	maxStaleMs    int64
	cookieName    string
	cookieMaxAge  int64
	cookieSecure  bool
	cookieDomain  string
	flushInterval time.Duration
	httpClient    HTTPDoer
	onError       func(error)
	now           func() time.Time
	sleep         func(time.Duration)
	fetchCtx      context.Context
	cancelFetches context.CancelFunc
	closed        atomic.Bool

	mu            sync.Mutex
	trust         *trustChain
	config        *config
	signingKey    *signingKey
	etag          string
	fetchedAt     int64
	flight        *flight
	failures      int
	retryAt       int64
	serverVersion int64
	catchUpAt     int64

	qmu       sync.Mutex
	queue     []wireEvent
	timer     *time.Timer
	exposed   map[string]struct{}
	flushTail chan struct{}

	scopeMu sync.Mutex
	scopes  map[*http.Request]*scope
}

type scope struct {
	sem    chan struct{}
	pinned bool
	cookie *scopeCookie
}

type scopeCookie struct {
	input         string
	userID        string
	assignmentKey string
	state         *assignmentState
}

// New returns a Client for opts. It fails only for invalid Options; an unreachable or
// misconfigured Leverprice never makes it fail.
func New(opts Options) (*Client, error) {
	guards, err := normalizeGuards(opts.Products)
	if err != nil {
		return nil, err
	}
	cookieName := orDefault(opts.CookieName, defaultCookieName)
	if !isCookieName(cookieName) {
		return nil, errors.New("leverprice: CookieName is not a valid cookie name")
	}
	c := &Client{
		guards:        guards,
		disabled:      opts.Disabled,
		apiURL:        strings.TrimRight(orDefault(opts.APIURL, defaultAPIURL), "/"),
		timeoutMs:     milliseconds(opts.Timeout, defaultTimeout),
		refreshMs:     milliseconds(opts.RefreshInterval, defaultRefreshInterval),
		maxStaleMs:    milliseconds(opts.MaxStale, defaultMaxStale),
		cookieName:    cookieName,
		cookieMaxAge:  int64(positive(opts.CookieMaxAge, defaultCookieMaxAge) / time.Second),
		cookieSecure:  opts.CookieSecure == nil || *opts.CookieSecure,
		cookieDomain:  opts.CookieDomain,
		flushInterval: positive(opts.FlushInterval, defaultFlushInterval),
		httpClient:    opts.HTTPClient,
		onError:       opts.OnError,
		now:           time.Now,
		sleep:         time.Sleep,
		exposed:       map[string]struct{}{},
		scopes:        map[*http.Request]*scope{},
	}
	if opts.Timeout < 0 {
		c.timeoutMs = 0
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	roots := opts.TrustedRootKeys
	if roots == nil {
		roots = RootPublicKeys()
	}
	c.trust = newTrustChain(roots, MinKeysetVersion)
	c.fetchCtx, c.cancelFetches = context.WithCancel(context.Background())

	key := jsTrim(opts.SecretKey)
	if isSecretKey(key) {
		c.secretKey = key
		c.fingerprint = sha256Hex(key)[:16]
	} else if key != "" && !c.disabled && !envDisabled() {
		c.emit(errors.New("leverprice: SecretKey is not a pp_sk_live_… / pp_sk_test_… key; serving fallback prices"))
	}
	return c, nil
}

func normalizeGuards(products map[string]ProductGuard) (map[string]guard, error) {
	out := make(map[string]guard, len(products))
	for key, g := range products {
		where := fmt.Sprintf("leverprice: Products[%q]", key)
		if !isCurrencyCode(g.Currency) {
			return nil, fmt.Errorf(`%s.Currency must be an ISO 4217 code such as "usd"`, where)
		}
		if g.FallbackPrice <= 0 || g.FallbackPrice > maxSafeInteger {
			return nil, fmt.Errorf("%s.FallbackPrice must be a positive integer (minor units)", where)
		}
		minPrice, maxPrice := g.MinPrice, g.MaxPrice
		if minPrice == 0 {
			minPrice = 1
		}
		if maxPrice == 0 {
			maxPrice = maxSafeInteger
		}
		if minPrice < -maxSafeInteger || maxPrice > maxSafeInteger || minPrice > maxPrice {
			return nil, fmt.Errorf("%s: MinPrice and MaxPrice must be at most 2^53-1 with MinPrice <= MaxPrice", where)
		}
		out[key] = guard{currency: asciiLower(g.Currency), fallbackPrice: g.FallbackPrice, minPrice: minPrice, maxPrice: maxPrice}
	}
	return out, nil
}

func isSecretKey(k string) bool {
	rest, ok := strings.CutPrefix(k, "pp_sk_live_")
	if !ok {
		rest, ok = strings.CutPrefix(k, "pp_sk_test_")
	}
	if !ok || len(rest) != 40 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func isCookieName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') &&
			!strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func positive(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func milliseconds(d, def time.Duration) int64 {
	return int64(positive(d, def) / time.Millisecond)
}

func (c *Client) nowMs() int64 { return c.now().UnixMilli() }

// GetPrice returns the price to show and charge for productKey. It fails only for a key missing
// from Options.Products; every Leverprice problem yields a fallback quote instead.
func (c *Client) GetPrice(ctx context.Context, productKey string, req PriceRequest) (*Quote, error) {
	g, ok := c.guards[productKey]
	if !ok {
		return nil, fmt.Errorf("%w %q; add it to Options.Products", ErrUnknownProduct, productKey)
	}
	if c.off() {
		return fallbackQuote(productKey, g, ReasonDisabled), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var sc *scope
	if req.Request != nil {
		sc = c.scopeFor(req.Request)
	}
	if sc != nil {
		select {
		case sc.sem <- struct{}{}:
			defer func() { <-sc.sem }()
		case <-ctx.Done():
			sc = nil
		}
	}
	return c.quote(ctx, productKey, g, &req, sc), nil
}

// scopeFor shares state between the quotes of one request until its context is done; a request
// whose context can never be done shares nothing.
func (c *Client) scopeFor(r *http.Request) *scope {
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if sc := c.scopes[r]; sc != nil {
		return sc
	}
	ctx := r.Context()
	if ctx.Done() == nil || len(c.scopes) >= maxScopes {
		return nil
	}
	sc := &scope{sem: make(chan struct{}, 1)}
	c.scopes[r] = sc
	context.AfterFunc(ctx, func() {
		c.scopeMu.Lock()
		if c.scopes[r] == sc {
			delete(c.scopes, r)
		}
		c.scopeMu.Unlock()
	})
	return sc
}

func (c *Client) quote(ctx context.Context, productKey string, g guard, pr *PriceRequest, sc *scope) (q *Quote) {
	bot := false
	fail := func(err error) *Quote {
		c.report(err)
		if bot {
			return fallbackQuote(productKey, g, ReasonBot)
		}
		return c.noConfig(productKey, g, sc)
	}
	defer func() {
		if r := recover(); r != nil {
			q = fail(fmt.Errorf("leverprice: internal error: %v", r))
		}
	}()
	info := readRequest(pr, c.cookieName)
	if pr.Bot != nil {
		bot = *pr.Bot
	} else {
		bot = IsBot(info.userAgent)
	}
	if bot {
		return c.botQuote(ctx, productKey, g)
	}
	if info.pinned || (sc != nil && sc.pinned) {
		c.mu.Lock()
		c.kickLocked(c.nowMs())
		c.mu.Unlock()
		return fallbackQuote(productKey, g, ReasonPinnedFallback, c.pinCookie())
	}
	cfg := c.currentConfig(ctx)
	if cfg == nil {
		return c.noConfig(productKey, g, sc)
	}
	p := cfg.products[productKey]
	switch {
	case p == nil:
		return fallbackQuote(productKey, g, ReasonUnknownProduct)
	case p.currency != g.currency:
		return fallbackQuote(productKey, g, ReasonCurrencyMismatch)
	case p.experiment == nil && inBounds(g, p, p.price):
		return priceQuote(productKey, g, p.price, ReasonBasePrice, p.checkoutURL)
	case p.experiment == nil || len(p.experiment.variants) == 0:
		return fallbackQuote(productKey, g, ReasonOutOfBounds)
	}
	for _, v := range p.experiment.variants {
		if !inBounds(g, p, v.price) {
			return fallbackQuote(productKey, g, ReasonOutOfBounds)
		}
	}
	q, err := c.experimentQuote(productKey, g, pr, info, sc, cfg, p)
	if err != nil {
		return fail(err)
	}
	return q
}

func (c *Client) experimentQuote(productKey string, g guard, pr *PriceRequest, info requestInfo, sc *scope,
	cfg *config, p *configProduct) (*Quote, error) {
	exp, key := p.experiment, cfg.key

	var state *assignmentState
	if prior := sc.priorCookie(info.cookieInput, pr.UserID, cfg.assignmentKey); prior != nil {
		state = prior.clone()
	} else {
		candidates := info.assignmentCookies
		if len(candidates) > 4 {
			candidates = candidates[:4]
		}
		for _, raw := range candidates {
			if state = verifyAssignment(key, raw); state != nil {
				break
			}
		}
	}
	changed := false
	if state == nil {
		v, err := randomHex(16)
		if err != nil {
			return nil, fmt.Errorf("leverprice: no random visitor key: %w", err)
		}
		state = newAssignmentState(v)
		changed = true
	}

	visitorKey := "a:" + state.v
	if pr.UserID != "" {
		visitorKey = "u:" + pr.UserID
	}
	visitorID := hmacHex(key, "pp:visitor:v1:"+visitorKey, 32)

	var chosen *variant
	if assigned, ok := state.variantOf(exp.id); ok {
		for i := range exp.variants {
			if exp.variants[i].id == assigned {
				chosen = &exp.variants[i]
				break
			}
		}
	}
	if chosen == nil {
		weights := make([]float64, len(exp.variants))
		for i, v := range exp.variants {
			weights[i] = v.weight
		}
		chosen = &exp.variants[pickVariant(weights, bucketFor(exp.salt, visitorID))]
		state.assign(exp.id, chosen.id)
		changed = true
	}

	logged := state.exposed(exp.id) || c.wasExposed(exp.id, visitorID)
	if !logged && pr.Exposure == ExposureAuto && !info.prefetch {
		c.recordExposure(exp.id, chosen.id, visitorID)
		logged = true
	}
	if logged && state.markExposed(exp.id) {
		changed = true
	}

	if sc != nil {
		sc.cookie = &scopeCookie{input: info.cookieInput, userID: pr.UserID, assignmentKey: cfg.assignmentKey, state: state}
	}
	setCookie := []string{}
	if changed {
		setCookie = append(setCookie, setCookieHeader(c.cookieName, signAssignment(key, state), c.cookieMaxAge, c.cookieSecure, c.cookieDomain))
	}
	checkoutURL := chosen.checkoutURL
	if checkoutURL == "" {
		checkoutURL = p.checkoutURL
	}
	signature := hmacHex(key, "pp:attr:v1:"+exp.id+":"+chosen.id+":"+visitorID, 16)
	return &Quote{
		ProductKey:   productKey,
		Price:        chosen.price,
		Currency:     g.currency,
		Reason:       ReasonExperiment,
		ExperimentID: exp.id,
		VariantID:    chosen.id,
		VisitorID:    visitorID,
		Metadata: map[string]string{
			"pp_exp": exp.id,
			"pp_var": chosen.id,
			"pp_vid": visitorID,
			"pp_sig": signature,
		},
		SetCookie:      setCookie,
		CheckoutURL:    checkoutURL,
		ExposureLogged: logged,
	}, nil
}

// priorCookie matches cookie input, user and key, so a request reused for many users shares nothing.
func (sc *scope) priorCookie(input, userID, assignmentKey string) *assignmentState {
	if sc == nil || sc.cookie == nil {
		return nil
	}
	if sc.cookie.input != input || sc.cookie.userID != userID || sc.cookie.assignmentKey != assignmentKey {
		return nil
	}
	return sc.cookie.state
}

func (c *Client) botQuote(ctx context.Context, productKey string, g guard) *Quote {
	if cfg := c.currentConfig(ctx); cfg != nil {
		if p := cfg.products[productKey]; p != nil && p.currency == g.currency && inBounds(g, p, p.price) {
			return priceQuote(productKey, g, p.price, ReasonBot, p.checkoutURL)
		}
	}
	return fallbackQuote(productKey, g, ReasonBot)
}

func (c *Client) noConfig(productKey string, g guard, sc *scope) *Quote {
	if sc != nil {
		sc.pinned = true
	}
	return fallbackQuote(productKey, g, ReasonNoConfig, c.pinCookie())
}

func (c *Client) pinCookie() string {
	return setCookieHeader(c.cookieName+"_f", "1", pinMaxAgeSeconds, c.cookieSecure, c.cookieDomain)
}

func inBounds(g guard, p *configProduct, price int64) bool {
	return price > 0 && price <= maxSafeInteger &&
		price >= max(g.minPrice, p.minPrice) && price <= min(g.maxPrice, p.maxPrice)
}

// LogExposure logs the exposure of a quote obtained with ExposureManual once its price is
// visible. It reports whether an exposure was queued and sets q.ExposureLogged.
func (c *Client) LogExposure(q *Quote) (queued bool) {
	defer func() {
		if r := recover(); r != nil {
			c.report(fmt.Errorf("leverprice: internal error: %v", r))
			queued = false
		}
	}()
	if q == nil || c.off() || q.Reason != ReasonExperiment || q.ExposureLogged {
		return false
	}
	if q.ExperimentID == "" || q.VariantID == "" || q.VisitorID == "" {
		return false
	}
	queued = c.recordExposure(q.ExperimentID, q.VariantID, q.VisitorID)
	q.ExposureLogged = true
	return queued
}

func envDisabled() bool {
	v := strings.ToLower(jsTrim(os.Getenv("LEVERPRICE_DISABLED")))
	return v == "1" || v == "true"
}

func (c *Client) off() bool {
	return c.disabled || c.secretKey == "" || envDisabled()
}

func (c *Client) report(err error) {
	if err != nil && !c.off() {
		c.emit(err)
	}
}

func (c *Client) emit(err error) {
	defer func() { _ = recover() }()
	if c.onError != nil {
		c.onError(err)
		return
	}
	slog.Warn(err.Error())
}
