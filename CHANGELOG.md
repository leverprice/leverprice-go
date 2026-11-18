# Changelog

All notable changes to this project are documented in this file. The project follows
[Semantic Versioning](https://semver.org).

## 0.4.1 — 2026-11-18

- Headers every browser sends (`Accept-Language`, `X-Forwarded-For`, `User-Agent`…) are never read
  as the visitor's country. Named in `Options.CountryHeader`, such a header is ignored and reported
  once through `OnError`; named in the project's settings, it is ignored too.
- VPN guard: a visitor keeps the price of a dearer country they were seen in for 7 days, so a VPN
  to a cheaper country does not lower it. The held country travels in the signed `__pp` cookie, so
  a visitor on a product with regional prices gets that cookie even without a test.
  `Quote.Country` is the country whose price applies.
- Visitors who refuse cookies keep one price and count once: without a cookie, the visitor is keyed
  by an HMAC of their IP address, browser and languages, made with your project's secret. Neither
  the address nor the browser leaves your server. Without an address the key stays random.
- Every config request names the products your code prices, and says why the last config was
  refused until one is accepted, so the dashboard can show whether visitors get real prices.
- Cookies stay compatible with 0.4.0 and 0.3: an older SDK drops the held country when it rewrites
  the cookie, which only ends the hold early.
- A cold start no longer gives up early: each request waits its own `Options.Timeout` for the
  first config, even when `Preload` or another request started the fetch earlier. Before, a busy
  server answered the first visitors with the fallback early, said nothing, and pinned them to it
  for 30 minutes. A request that does give up now reports `leverprice config: still loading after
  500 ms (Timeout); …` to `OnError`, once per fetch.

## 0.4.0 — 2026-11-18

- Settings live in Leverprice, not in code: the product's price range and the header that carries
  the visitor's country (Project settings → Visitor country) arrive in the signed config.
  `MinPrice` and `MaxPrice` in code are no longer used; 0.3 code that sets them keeps compiling and
  running.
- Upgrading: limits set in code (`MinPrice`, `MaxPrice`) no longer hold prices. If they were tighter
  than the product's range in Leverprice, tighten the range in the dashboard before you upgrade.
- Regional prices: a product can cost something else in each country, in your currency or the
  visitor's. `PriceRequest.Country`, `Country` / `Regional` on the quote, and an
  `Options.CountryHeader` that overrides the project setting. Prices in the visitor's currency
  need `Options.LocalCurrencies` (set it when your checkout charges `Quote.Currency`);
  `Quote.Currency` is then the region's currency.
- Exposures carry the visitor's country when it is known.
- Tests by country: a test can run in selected countries only, and countries can be split between
  the prices their own way. Both are set in the dashboard; the SDK only needs the visitor's country.
- Cookies and the wire protocol are compatible with 0.3.
- Turbo 8 hover prefetches (`X-Sec-Purpose: prefetch`) are priced but no longer counted as
  exposures, like other prefetches.
- README: the Stripe example uses the stripe-go v87 client (`stripe.NewClient`,
  `V1CheckoutSessions.Create`). The webhook counts only paid Checkout Sessions, handles
  `checkout.session.async_payment_succeeded`, reports a subscription's first payment by its
  PaymentIntent id (as verified webhooks do) and skips $0 sessions.
- `ProviderPaddle` and `ProviderCreem` for `TrackPurchase` and `TrackRefund`.

## 0.3.0 — 2026-11-17

First release of the Go SDK, at parity with the other Leverprice SDKs (protocol v2).

- `Client.GetPrice` with signed configs (root → keyset → config), local guardrails, sticky
  HMAC-signed assignment cookies, outage pinning, bot detection and prefetch handling.
- `TrackPurchase`, `TrackRefund` and `LogExposure` with batched, retried event delivery.
- `Flush`, `Refresh`, `Preload` and `Close` for lifecycle control; `LEVERPRICE_DISABLED` kill switch.
- Passes the shared conformance vectors.
