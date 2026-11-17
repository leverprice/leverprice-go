<p>
<a href="https://leverprice.com">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/leverprice/.github/main/assets/repo/go-dark.svg">
    <img alt="Leverprice SDK for Go" src="https://raw.githubusercontent.com/leverprice/.github/main/assets/repo/go-light.svg" width="100%">
  </picture>
</a>
</p>

<p>
  <a href="https://pkg.go.dev/github.com/leverprice/leverprice-go"><img alt="Go reference" src="https://img.shields.io/badge/go.dev-reference-5B4CF5?style=flat-square&labelColor=0F1011"></a>
  <a href="https://github.com/leverprice/leverprice-go/tags"><img alt="Version" src="https://img.shields.io/github/v/tag/leverprice/leverprice-go?label=version&style=flat-square&labelColor=0F1011&color=5B4CF5"></a>
  <a href="https://github.com/leverprice/leverprice-go/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/leverprice/leverprice-go/ci.yml?branch=main&label=ci&style=flat-square&labelColor=0F1011"></a>
  <a href="https://github.com/leverprice/leverprice-go/blob/main/LICENSE"><img alt="MIT license" src="https://img.shields.io/badge/license-MIT-5B4CF5?style=flat-square&labelColor=0F1011"></a>
</p>

Server-side SDK for [Leverprice](https://leverprice.com) price experiments. Go 1.22+, standard library only.

```bash
go get github.com/leverprice/leverprice-go
```

Also available for [JavaScript](https://github.com/leverprice/leverprice-node), [Python](https://github.com/leverprice/leverprice-python), [PHP](https://github.com/leverprice/leverprice-php) and [Ruby](https://github.com/leverprice/leverprice-ruby). Every SDK passes the same conformance vectors (`conformance/vectors.json`).

**Your payments stay yours.** The SDK hands your server a number. Your own code creates the checkout with your own provider keys; Leverprice never sees card data or provider API keys.

## Quick start

```go
import "github.com/leverprice/leverprice-go"

lp, err := leverprice.New(leverprice.Options{
	SecretKey: os.Getenv("LEVERPRICE_SECRET_KEY"), // server-side only
	Products: map[string]leverprice.ProductGuard{
		// Local guardrails: prices outside these bounds are never returned.
		"pro": {Currency: "usd", FallbackPrice: 1900, MinPrice: 1500, MaxPrice: 3900},
	},
})
if err != nil {
	log.Fatal(err) // invalid Options only
}
defer lp.Close(context.Background()) // sends queued events
lp.Preload(context.Background())      // optional: fetch the config before the first request
```

### net/http and Chi

```go
func pricing(w http.ResponseWriter, r *http.Request) {
	q, err := lp.GetPrice(r.Context(), "pro", leverprice.PriceRequest{Request: r, UserID: userID(r)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError) // unknown product key
		return
	}
	q.WriteCookies(w) // before writing the body
	render(w, q.Price) // minor units
}

http.HandleFunc("/pricing", pricing) // or chi: r.Get("/pricing", pricing)
```

### Gin

```go
router.GET("/pricing", func(c *gin.Context) {
	q, err := lp.GetPrice(c.Request.Context(), "pro", leverprice.PriceRequest{Request: c.Request})
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	q.WriteCookies(c.Writer)
	c.JSON(http.StatusOK, gin.H{"price": q.Price, "currency": q.Currency})
})
```

### Echo

```go
e.GET("/pricing", func(c echo.Context) error {
	q, err := lp.GetPrice(c.Request().Context(), "pro", leverprice.PriceRequest{Request: c.Request()})
	if err != nil {
		return err
	}
	q.WriteCookies(c.Response())
	return c.JSON(http.StatusOK, map[string]any{"price": q.Price, "currency": q.Currency})
})
```

Other frameworks: pass `Header` (an `http.Header`) instead of `Request`, and add every `q.SetCookie` value as a `Set-Cookie` response header.

### Stripe checkout and webhook (stripe-go)

Price again where you create the checkout. Never trust a price sent by the browser.

```go
q, err := lp.GetPrice(r.Context(), "pro", leverprice.PriceRequest{Request: r})
if err != nil {
	return err
}
q.WriteCookies(w)
params := &stripe.CheckoutSessionParams{
	Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
	LineItems: []*stripe.CheckoutSessionLineItemParams{{
		Quantity: stripe.Int64(1),
		PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
			Currency:    stripe.String(q.Currency),
			UnitAmount:  stripe.Int64(q.Price),
			ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{Name: stripe.String("Pro")},
		},
	}},
	Metadata:          q.Metadata, // signed attribution: pass it whole
	PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{Metadata: q.Metadata},
	SuccessURL:        stripe.String(successURL),
	CancelURL:         stripe.String(cancelURL),
}
s, err := session.New(params)
```

Report the purchase from your webhook handler (or connect your provider's webhooks to Leverprice directly):

```go
event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), endpointSecret)
if err != nil {
	http.Error(w, err.Error(), http.StatusBadRequest)
	return
}
if event.Type == stripe.EventTypeCheckoutSessionCompleted {
	var s stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var tax int64
	if s.TotalDetails != nil {
		tax = s.TotalDetails.AmountTax
	}
	lp.TrackPurchase(r.Context(), leverprice.Purchase{
		Provider:  leverprice.ProviderStripe,
		OrderID:   s.PaymentIntent.ID,
		Amount:    s.AmountTotal - tax,
		Currency:  string(s.Currency),
		Metadata:  s.Metadata,
		Timestamp: s.Created, // Unix seconds are converted
	})
}
w.WriteHeader(http.StatusOK)
```

## Guarantees

- **Fails safe.** If Leverprice is off, slow (no config within `Timeout`, 500 ms by default), down, misconfigured or returns anything unexpected, `GetPrice` returns your `FallbackPrice`. Its only error is an unknown product key, a bug in the calling code. `TrackPurchase`, `TrackRefund`, `LogExposure`, `Flush`, `Refresh`, `Preload` and `Close` never fail and never panic.
- **Verified config.** Root key → keyset → config signature chain (Ed25519), version-monotonic (no rollback, no revoked keys), time-limited.
- **Bounded.** Every price is checked against your local `MinPrice`/`MaxPrice` and `Currency`.
- **Sticky and tamper-proof.** Visitors keep their price. The assignment cookie is HMAC-signed with your project's assignment key, delivered in the signed config, so rotating your API key does not reshuffle visitors.
- **Private.** Visitor and user ids are HMAC-hashed on your server; Leverprice only sees the hash.
- **No price flips.** A visitor who got the fallback during an outage keeps it for the rest of their session.
- **Search engines see the standard price.** Crawlers and link previews are never put into experiments.
- **Goroutine-safe, nothing left running.** One `Client` serves the whole process. Config refreshes are driven by traffic and event flushes by a timer; `Close` stops both.

## Requests and cookies

Pass the incoming `*http.Request` (or just its `Header`). The SDK reads the `Cookie` header, the `User-Agent` and prefetch hints. `Cookie` and `UserAgent` in `PriceRequest` override what is read from the request.

`q.SetCookie` is a **slice** of `Set-Cookie` header values: usually empty, one value when the assignment cookie (`__pp`) changes, or the outage pin (`__pp_f`). `q.WriteCookies(w)` adds them all.

Pricing several products for one page? Pass the **same `*http.Request`** to each `GetPrice` call: the quotes share one visitor cookie and the last quote's cookie carries every assignment. Sharing lasts until the request's context is done, which `net/http` does when the handler returns.

`ctx` bounds the cold-start wait together with `Timeout`; pass `r.Context()`.

## Exposures

An exposure is one person seeing an experiment price. With `ExposureAuto` (the default) the SDK logs it the first time a visitor gets a price from an experiment, once per visitor and experiment (remembered in the cookie; the server deduplicates too). Prefetch and prerender requests (`Sec-Purpose`, `Purpose`, `X-Purpose`, `X-Moz`, `Next-Router-Prefetch`) are priced but not counted.

If you compute the price before it is visible, use manual mode:

```go
q, _ := lp.GetPrice(ctx, "pro", leverprice.PriceRequest{Request: r, Exposure: leverprice.ExposureManual})
// later, when the price is actually on screen:
lp.LogExposure(q)
```

Exposures are sent in the background within `FlushInterval` (2 s). Call `lp.Flush(ctx)` or `lp.Close(ctx)` before a short-lived process exits.

## Bots

Crawlers, link previewers (Slack, WhatsApp, iMessage…), headless browsers, uptime and performance monitors and command-line clients get the product's standard price (or your fallback if no config is available) with reason `bot`: no assignment, no cookie, no exposure. An empty user agent counts as a person. Bring your own detection with `PriceRequest{Bot: &verdict}`; the built-in check is exported as `leverprice.IsBot(userAgent)`.

## Kill switch

Set `LEVERPRICE_DISABLED=1` (or `true`) in the environment, or pass `Disabled: true`. The variable is read on every call. While disabled, `GetPrice` returns your fallback price with reason `disabled`, sets no cookies, makes no network calls and reports nothing; tracking and exposure calls do nothing. A missing or malformed `SecretKey` behaves the same way (a malformed key is reported once through `OnError`).

## Outages

When no usable config is available (reason `no_config`), the quote also sets `__pp_f=1` for 30 minutes. While a browser sends it, it gets the fallback price with reason `pinned_fallback` and is not enrolled; each pinned request extends it, so the price changes only after 30 minutes of inactivity. The cookie is unsigned on purpose: it can only select your own fallback price.

Failed config fetches back off exponentially (up to 30 s). Undelivered purchases and refunds stay queued and are retried on the next flush; exposures are best-effort.

## Trust chain

The SDK embeds only Leverprice's offline root public keys (`leverprice.RootPublicKeys()`). Each config carries a keyset of online signing keys signed by a root key, and must be signed by one of those keys. Keyset versions only move forward within a process (a revoked key cannot come back with an older keyset, and a config it signed stops being trusted); `MinKeysetVersion` sets the floor for a new client. Config versions only move forward, configs expire, and a config is accepted only if it names your API key. For staging or local development pass `TrustedRootKeys: map[string]string{"r1": "<root public key>"}`.

The config is revalidated every 30 s with an ETag. When any Leverprice response announces a newer version in `Leverprice-Config-Version`, the SDK fetches it right away.

## Reference

`GetPrice(ctx, productKey, PriceRequest) (*Quote, error)`: `PriceRequest` has `Request`, `Header`, `Cookie`, `UserAgent`, `UserID`, `Exposure` and `Bot`.

| Quote field | |
|---|---|
| `ProductKey` | The key you asked for |
| `Price` | Minor units; use it for display and as the checkout amount |
| `Currency` | Lowercase ISO 4217, from your product guard |
| `IsFallback` | `Price` is your `FallbackPrice` |
| `Reason` | See below |
| `ExperimentID`, `VariantID` | Set when `Reason` is `experiment`, else `""` |
| `VisitorID` | Hashed visitor id Leverprice sees; `""` unless `Reason` is `experiment` |
| `Metadata` | `pp_exp`, `pp_var`, `pp_vid`, `pp_sig` for the checkout (signed); empty otherwise |
| `SetCookie` | `Set-Cookie` values for the response (zero or more) |
| `CheckoutURL` | No-code mode: hosted checkout link for this price, or `""` |
| `ExposureLogged` | An exposure for this visitor and experiment has been logged (now or earlier) |

Quotes encode to JSON with the same field names and `null` for empty ids and `CheckoutURL`, as in the other SDKs.

| `Reason` | Price |
|---|---|
| `experiment` | The visitor's sticky variant |
| `base_price` | Standard price (no experiment running) |
| `bot` | Standard price, or fallback if unavailable; never enrolled |
| `disabled` | Fallback: kill switch, or no valid secret key |
| `no_config` | Fallback: no usable signed config (timeout, outage, rejected or expired); sets the pin |
| `pinned_fallback` | Fallback: this browser got a fallback during an outage in this session |
| `unknown_product` | Fallback: the config has no such product |
| `currency_mismatch` | Fallback: the config's currency differs from your guard |
| `out_of_bounds` | Fallback: a price is outside your (or Leverprice's) min/max |

| Method | |
|---|---|
| `LogExposure(q)` | Logs a manual-mode exposure; reports whether one was queued |
| `TrackPurchase(ctx, Purchase)` | Reads `pp_exp`/`pp_var`/`pp_vid`/`pp_sig` from `Metadata`; sends at once |
| `TrackRefund(ctx, Refund)` | `RefundID` + `Amount`, or `RefundedTotal`; sends at once |
| `Flush(ctx)` | Sends queued events (≤ 500 per request; money events are retried and kept on failure) |
| `Refresh(ctx)` / `Preload(ctx)` | Fetch the config now / only if none is usable |
| `Close(ctx)` | Flushes and stops background work |

| Option | Default |
|---|---|
| `SecretKey` | Required; `pp_sk_live_…` or `pp_sk_test_…` |
| `Products` | Required; `ProductGuard{Currency, FallbackPrice, MinPrice, MaxPrice}` by key |
| `Disabled` | `false` |
| `APIURL` | `https://api.leverprice.com` |
| `Timeout` | 500 ms (negative: never wait) |
| `RefreshInterval` | 30 s |
| `MaxStale` | 24 h |
| `CookieName` | `__pp` |
| `CookieMaxAge` | 180 days |
| `CookieSecure` | `true` |
| `CookieDomain` | none |
| `FlushInterval` | 2 s |
| `TrustedRootKeys` | `RootPublicKeys()` |
| `HTTPClient` | a plain `*http.Client` (any `Do(*http.Request)` implementation) |
| `OnError` | a `log/slog` warning |

Error messages start with `leverprice`.

## License

MIT
