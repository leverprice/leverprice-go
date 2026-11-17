// Package leverprice is the server-side SDK for Leverprice price experiments.
//
// A Client turns a product key and the incoming request into a Quote: the price to show and to
// charge. Prices come only from a config signed through Leverprice's root → keyset → config key
// chain and are always checked against the local guardrails in Options.Products. Whenever
// Leverprice is off, slow, unreachable or misconfigured, GetPrice returns the product's
// FallbackPrice instead of failing.
//
//	lp, err := leverprice.New(leverprice.Options{
//		SecretKey: os.Getenv("LEVERPRICE_SECRET_KEY"),
//		Products: map[string]leverprice.ProductGuard{
//			"pro": {Currency: "usd", FallbackPrice: 1900, MinPrice: 1500, MaxPrice: 3900},
//		},
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer lp.Close(context.Background())
//
//	http.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
//		q, err := lp.GetPrice(r.Context(), "pro", leverprice.PriceRequest{Request: r})
//		if err != nil {
//			http.Error(w, err.Error(), http.StatusInternalServerError)
//			return
//		}
//		q.WriteCookies(w)
//		fmt.Fprintf(w, "%d %s", q.Price, q.Currency)
//	})
//
// Pass Quote.Metadata to the checkout and back to Client.TrackPurchase from the payment
// webhook to attribute revenue to the experiment variant. Visitor and user ids leave the server
// only as HMACs.
//
// A Client is safe for concurrent use and starts no goroutine of its own until it is used; call
// Close before the process exits to send queued events.
package leverprice
