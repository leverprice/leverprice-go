package leverprice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the SDK version.
const Version = "0.4.1"

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

	productsHeader      = "Leverprice-Products"
	configErrorHeader   = "Leverprice-Config-Error"
	maxReportedProducts = 50
)

// HTTPDoer sends HTTP requests; *http.Client implements it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ProductGuard holds a product's currency and fallback price. Prices are in minor units.
type ProductGuard struct {
	Currency      string // ISO 4217 code, e.g. "usd"
	FallbackPrice int64  // served whenever Leverprice cannot price the product; must be positive

	// Deprecated: ignored since 0.4; the price range is set in Leverprice.
	MinPrice int64

	// Deprecated: ignored since 0.4; the price range is set in Leverprice.
	MaxPrice int64
}

// Options configure a Client. Zero values select the defaults.
type Options struct {
	SecretKey       string                  // pp_sk_live_… or pp_sk_test_…; missing or malformed disables the SDK
	Products        map[string]ProductGuard // currency and fallback price by product key
	Disabled        bool                    // kill switch; LEVERPRICE_DISABLED=1|true does the same
	APIURL          string                  // default https://api.leverprice.com
	Timeout         time.Duration           // cold-start wait for a config; default 500ms, negative for none
	RefreshInterval time.Duration           // config revalidation period; default 30s
	MaxStale        time.Duration           // stop using a config not confirmed for this long; default 24h
	CookieName      string                  // assignment cookie name; default "__pp" (the pin is CookieName+"_f")
	CookieMaxAge    time.Duration           // assignment cookie lifetime; default 180 days
	CookieSecure    *bool                   // Secure cookie attribute; default true
	CookieDomain    string                  // Domain cookie attribute; default none
	CountryHeader   string                  // trusted header with the visitor's country, e.g. "CF-IPCountry"; overrides the project's setting; a header browsers send is ignored and reported
	LocalCurrencies bool                    // also serve regional prices in visitors' own currencies; set only if checkout charges Quote.Currency
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
	Country   string        // visitor's country (ISO 3166-1 alpha-2); wins over every country header
	Exposure  ExposureMode
	Bot       *bool // own bot verdict; skips the built-in user-agent check
}

type guard struct {
	currency      string
	fallbackPrice int64
}

// Client prices products and reports events. It is safe for concurrent use; create one per
// process and share it.
type Client struct {
	secretKey       string
	fingerprint     string
	guards          map[string]guard
	disabled        bool
	apiURL          string
	timeoutMs       int64
	refreshMs       int64
	maxStaleMs      int64
	cookieName      string
	cookieMaxAge    int64
	cookieSecure    bool
	cookieDomain    string
	countryHeader   string
	localCurrencies bool
	productsHeader  string
	flushInterval   time.Duration
	httpClient      HTTPDoer
	onError         func(error)
	now             func() time.Time
	sleep           func(time.Duration)
	fetchCtx        context.Context
	cancelFetches   context.CancelFunc
	closed          atomic.Bool

	mu            sync.Mutex
	trust         *trustChain
	config        *config
	configError   string // the code of the last refused config, until one is accepted
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
	if !isToken(cookieName) {
		return nil, errors.New("leverprice: CookieName is not a valid cookie name")
	}
	countryHeader := trimASCIISpace(opts.CountryHeader)
	if countryHeader != "" && !isToken(countryHeader) {
		return nil, errors.New("leverprice: CountryHeader is not a valid header name")
	}
	c := &Client{
		guards:          guards,
		disabled:        opts.Disabled,
		apiURL:          strings.TrimRight(orDefault(opts.APIURL, defaultAPIURL), "/"),
		timeoutMs:       milliseconds(opts.Timeout, defaultTimeout),
		refreshMs:       milliseconds(opts.RefreshInterval, defaultRefreshInterval),
		maxStaleMs:      milliseconds(opts.MaxStale, defaultMaxStale),
		cookieName:      cookieName,
		cookieMaxAge:    int64(positive(opts.CookieMaxAge, defaultCookieMaxAge) / time.Second),
		cookieSecure:    opts.CookieSecure == nil || *opts.CookieSecure,
		cookieDomain:    opts.CookieDomain,
		localCurrencies: opts.LocalCurrencies,
		productsHeader:  productsHeaderValue(guards),
		flushInterval:   positive(opts.FlushInterval, defaultFlushInterval),
		httpClient:      opts.HTTPClient,
		onError:         opts.OnError,
		now:             time.Now,
		sleep:           time.Sleep,
		exposed:         map[string]struct{}{},
		scopes:          map[*http.Request]*scope{},
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

	quiet := c.disabled || envDisabled()
	key := jsTrim(opts.SecretKey)
	if isSecretKey(key) {
		c.secretKey = key
		c.fingerprint = sha256Hex(key)[:16]
	} else if key != "" && !quiet {
		c.emit(errors.New("leverprice: SecretKey is not a pp_sk_live_… / pp_sk_test_… key; serving fallback prices"))
	}
	// A header the visitor sends would let them pick their own price: the project's setting is used.
	named := asciiLower(countryHeader)
	if named != "" && visitorSetHeader(named) {
		if !quiet {
			c.emit(fmt.Errorf("leverprice: CountryHeader %q is sent by visitors' browsers, so it would let them pick their own "+
				"price; it is ignored. Name the header your CDN or host sets, such as cf-ipcountry.", named))
		}
		named = ""
	}
	c.countryHeader = named
	return c, nil
}

// productsHeaderValue lists key=currency per product, by key, keys percent-encoded (every byte but
// A-Z a-z 0-9 - . _ ~), at most maxReportedProducts.
func productsHeaderValue(guards map[string]guard) string {
	entries := make([]string, 0, len(guards))
	currencyOf := make(map[string]string, len(guards))
	for key, g := range guards {
		encoded := encodeProductKey(key)
		entries = append(entries, encoded)
		currencyOf[encoded] = g.currency
	}
	sort.Strings(entries)
	if len(entries) > maxReportedProducts {
		entries = entries[:maxReportedProducts]
	}
	for i, key := range entries {
		entries[i] = key + "=" + currencyOf[key]
	}
	return strings.Join(entries, ", ")
}

func encodeProductKey(key string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	return b.String()
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
		out[key] = guard{currency: asciiLower(g.Currency), fallbackPrice: g.FallbackPrice}
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

func isToken(s string) bool {
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
	info := readRequest(pr, c.cookieName, c.countryHeader)
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
	if pr.Country == "" && c.countryHeader == "" && cfg.countryHeader != "" {
		info.country = headerCountry(requestHeader(pr), cfg.countryHeader)
	}
	p := cfg.products[productKey]
	switch {
	case p == nil:
		return fallbackQuote(productKey, g, ReasonUnknownProduct)
	case p.currency != g.currency:
		return fallbackQuote(productKey, g, ReasonCurrencyMismatch)
	}
	// The country a visitor holds lives in the cookie, so it is read before the test is chosen.
	country := info.country
	var held *visitor
	if cfg.countryHold > 0 && len(p.regions) > 0 {
		var err error
		if held, err = c.visitorFor(pr, info, sc, cfg); err != nil {
			return fail(err)
		}
		country = c.heldCountry(held, g, p, country, cfg.countryHold)
	}
	exp := experimentFor(p, country)
	switch {
	case exp == nil && inBounds(p, p.price):
		if r := c.regionFor(g, p, country); r != nil {
			q = regionalQuote(productKey, g, r)
		} else {
			q = priceQuote(productKey, g, p.price, ReasonBasePrice, p.checkoutURL)
		}
		q.Country = country
		if held != nil {
			cookies := c.keep(held, sc, info, cfg)
			// Outside a test the cookie only carries the held country: none held, none sent.
			if held.state.c != "" {
				q.SetCookie = cookies
			}
		}
		return q
	case exp == nil || len(exp.variants) == 0:
		return fallbackQuote(productKey, g, ReasonOutOfBounds)
	}
	for _, v := range exp.variants {
		if !inBounds(p, v.price) {
			return fallbackQuote(productKey, g, ReasonOutOfBounds)
		}
	}
	vis := held
	if vis == nil {
		var err error
		if vis, err = c.visitorFor(pr, info, sc, cfg); err != nil {
			return fail(err)
		}
	}
	return c.experimentQuote(productKey, g, pr, info, sc, cfg, p, exp, c.regionFor(g, p, country), country, vis)
}

// experimentFor returns the test a visitor from country is in, or nil. A scoped test runs only
// inside its scope: anyone else, an unknown country included, is priced as if there were no test.
func experimentFor(p *configProduct, country string) *experiment {
	exp := p.experiment
	if exp == nil || exp.scope == nil {
		return exp
	}
	if _, inScope := exp.scope[country]; country == "" || !inScope {
		return nil
	}
	return exp
}

// regionFor returns the visitor's region when its price may be served: the product has one for the
// country, in the guard's currency unless LocalCurrencies is set.
func (c *Client) regionFor(g guard, p *configProduct, country string) *region {
	if country == "" {
		return nil
	}
	i, ok := p.regionOf[country]
	if !ok {
		return nil
	}
	r := &p.regions[i]
	if r.currency != g.currency && !c.localCurrencies {
		return nil
	}
	return r
}

// visitor is the visitor behind a request and the cookie state to send back.
type visitor struct {
	userID  string
	state   *assignmentState
	changed bool // the state differs from the request's cookie: send a new one
}

// visitorFor returns the state an earlier quote of the same request left, else the first valid
// assignment cookie, else a new visitor.
func (c *Client) visitorFor(pr *PriceRequest, info requestInfo, sc *scope, cfg *config) (*visitor, error) {
	v := &visitor{userID: pr.UserID}
	if prior := sc.priorCookie(info.cookieInput, pr.UserID, cfg.assignmentKey); prior != nil {
		v.state = prior.clone()
		return v, nil
	}
	candidates := info.assignmentCookies
	if len(candidates) > 4 {
		candidates = candidates[:4]
	}
	for _, raw := range candidates {
		if v.state = verifyAssignment(cfg.key, raw); v.state != nil {
			return v, nil
		}
	}
	key, err := newVisitorKey(cfg.key, pr, info)
	if err != nil {
		return nil, fmt.Errorf("leverprice: no random visitor key: %w", err)
	}
	v.state, v.changed = newAssignmentState(key), true
	return v, nil
}

// newVisitorKey returns a new visitor's key. Without a cookie a browser comes back as a new
// visitor every time, so the key comes from what stays the same between its requests (address,
// browser, languages), keyed by the project's secret: a visitor who refuses cookies keeps one
// price and counts once. Neither the address nor the browser leaves the server. Without an
// address, a random key.
func newVisitorKey(key []byte, pr *PriceRequest, info requestInfo) (string, error) {
	address, ok := clientAddress(pr)
	if !ok {
		return randomHex(16)
	}
	language, _ := headerValue(requestHeader(pr), "accept-language")
	return hmacHex(key, anonMessage(address, info.userAgent, language), 32), nil
}

func anonMessage(address, userAgent, acceptLanguage string) string {
	return "pp:anon:v1:" + address + "\n" + userAgent + "\n" + acceptLanguage
}

// heldCountry returns the country whose price the visitor gets (Leverprice's VPN guard). A
// visitor seen lately in a country that pays more keeps that country's price, so switching a VPN
// to a cheaper country changes nothing until the hold runs out. Anywhere else, the visitor is
// priced where they are and that country is held from now on.
func (c *Client) heldCountry(v *visitor, g guard, p *configProduct, country string, holdSeconds int64) string {
	now := c.now().Unix()
	held := ""
	if s := v.state; s.c != "" && now-s.t < holdSeconds {
		held = s.c
	}
	if held != "" && held != country && c.levelOf(g, p, held) > c.levelOf(g, p, country) {
		return held
	}
	if country != "" && held != country {
		v.state.c, v.state.t = country, now
		v.changed = true
	}
	return country
}

// levelOf returns what a visitor from country pays, as basis points of the standard price.
func (c *Client) levelOf(g guard, p *configProduct, country string) int64 {
	if r := c.regionFor(g, p, country); r != nil {
		return r.level
	}
	return standardLevel
}

// keep remembers the visitor for the request's next quote and returns the Set-Cookie values when
// the state changed.
func (c *Client) keep(v *visitor, sc *scope, info requestInfo, cfg *config) []string {
	if sc != nil {
		sc.cookie = &scopeCookie{input: info.cookieInput, userID: v.userID, assignmentKey: cfg.assignmentKey, state: v.state}
	}
	if !v.changed {
		return []string{}
	}
	return []string{setCookieHeader(c.cookieName, signAssignment(cfg.key, v.state), c.cookieMaxAge, c.cookieSecure, c.cookieDomain)}
}

func (c *Client) experimentQuote(productKey string, g guard, pr *PriceRequest, info requestInfo, sc *scope,
	cfg *config, p *configProduct, exp *experiment, r *region, country string, vis *visitor) *Quote {
	key, state := cfg.key, vis.state
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
		chosen = &exp.variants[pickVariant(exp.weightsFor(country), bucketFor(exp.salt, visitorID))]
		state.assign(exp.id, chosen.id)
		vis.changed = true
	}

	logged := state.exposed(exp.id) || c.wasExposed(exp.id, visitorID)
	if !logged && pr.Exposure == ExposureAuto && !info.prefetch {
		c.recordExposure(exp.id, chosen.id, visitorID, country)
		logged = true
	}
	if logged && state.markExposed(exp.id) {
		vis.changed = true
	}

	setCookie := c.keep(vis, sc, info, cfg)
	price, currency, checkoutURL := chosen.price, g.currency, chosen.checkoutURL
	if checkoutURL == "" {
		checkoutURL = p.checkoutURL
	}
	if r != nil {
		price, currency, checkoutURL = chosen.regional[r.index], r.currency, ""
	}
	signature := hmacHex(key, "pp:attr:v1:"+exp.id+":"+chosen.id+":"+visitorID, 16)
	return &Quote{
		ProductKey:   productKey,
		Price:        price,
		Currency:     currency,
		Country:      country,
		Regional:     r != nil,
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
	}
}

// weightsFor returns the weights that split the visitors from country between the variants: the
// country's own when it has a split, else the variants'.
func (e *experiment) weightsFor(country string) []float64 {
	if weights, split := e.splitOf[country]; split && country != "" {
		return weights
	}
	weights := make([]float64, len(e.variants))
	for i, v := range e.variants {
		weights[i] = v.weight
	}
	return weights
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
		if p := cfg.products[productKey]; p != nil && p.currency == g.currency && inBounds(p, p.price) {
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

// inBounds reports whether a standard or test price is inside the product's signed price range.
func inBounds(p *configProduct, price int64) bool {
	return price > 0 && price <= maxSafeInteger && price >= p.minPrice && price <= p.maxPrice
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
	queued = c.recordExposure(q.ExperimentID, q.VariantID, q.VisitorID, normalizeCountry(q.Country))
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
