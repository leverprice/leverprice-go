package leverprice_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/leverprice/leverprice-go"
)

func Example() {
	lp, err := leverprice.New(leverprice.Options{
		SecretKey: os.Getenv("LEVERPRICE_SECRET_KEY"),
		Products: map[string]leverprice.ProductGuard{
			"pro": {Currency: "usd", FallbackPrice: 1900},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lp.Close(context.Background())
	lp.Preload(context.Background())

	http.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		q, err := lp.GetPrice(r.Context(), "pro", leverprice.PriceRequest{Request: r})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q.WriteCookies(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"price": q.Price, "currency": q.Currency})
	})
}

func Example_regionalPrices() {
	lp, err := leverprice.New(leverprice.Options{
		SecretKey:       os.Getenv("LEVERPRICE_SECRET_KEY"),
		LocalCurrencies: true,
		Products: map[string]leverprice.ProductGuard{
			"pro": {Currency: "usd", FallbackPrice: 2900},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lp.Close(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		q, err := lp.GetPrice(r.Context(), "pro", leverprice.PriceRequest{Request: r})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q.WriteCookies(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"price":    q.Price,
			"currency": q.Currency,
			"country":  q.Country,
			"regional": q.Regional,
		})
	})
}

func ExampleProductGuard() {
	lp, err := leverprice.New(leverprice.Options{
		SecretKey: os.Getenv("LEVERPRICE_SECRET_KEY"),
		Products: map[string]leverprice.ProductGuard{
			"pro":  {Currency: "usd", FallbackPrice: 2900},
			"team": {Currency: "eur", FallbackPrice: 9900},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lp.Close(context.Background())
}

func ExampleClient_GetPrice() {
	lp, err := leverprice.New(leverprice.Options{
		Products: map[string]leverprice.ProductGuard{
			"pro": {Currency: "usd", FallbackPrice: 1900},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/pricing", nil)
	q, err := lp.GetPrice(context.Background(), "pro", leverprice.PriceRequest{Request: r})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(q.Price, q.Currency, q.IsFallback, q.Reason)
	// Output: 1900 usd true disabled
}

func ExampleClient_TrackPurchase() {
	lp, err := leverprice.New(leverprice.Options{
		SecretKey: os.Getenv("LEVERPRICE_SECRET_KEY"),
		Products:  map[string]leverprice.ProductGuard{"pro": {Currency: "usd", FallbackPrice: 1900}},
	})
	if err != nil {
		log.Fatal(err)
	}
	lp.TrackPurchase(context.Background(), leverprice.Purchase{
		Provider: leverprice.ProviderStripe,
		OrderID:  "pi_3Q0example",
		Amount:   2900,
		Currency: "usd",
		Metadata: map[string]string{"pp_exp": "exp_A", "pp_var": "var_2", "pp_vid": "ad4f85c841e8485cc9a683a318147ba0"},
	})
}
