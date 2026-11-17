# Changelog

All notable changes to this project are documented in this file. The project follows
[Semantic Versioning](https://semver.org).

## 0.3.0 - 2026-11-17

First release of the Go SDK, at parity with the other Leverprice SDKs (protocol v2).

- `Client.GetPrice` with signed configs (root → keyset → config), local guardrails, sticky
  HMAC-signed assignment cookies, outage pinning, bot detection and prefetch handling.
- `TrackPurchase`, `TrackRefund` and `LogExposure` with batched, retried event delivery.
- `Flush`, `Refresh`, `Preload` and `Close` for lifecycle control; `LEVERPRICE_DISABLED` kill switch.
- Passes the shared conformance vectors.
