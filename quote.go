package leverprice

import (
	"encoding/json"
	"net/http"
)

// Reason explains why a Quote has its price.
type Reason string

// Quote reasons.
const (
	ReasonExperiment       Reason = "experiment"        // the visitor's sticky experiment variant
	ReasonBasePrice        Reason = "base_price"        // the product's price; no experiment runs for this visitor
	ReasonBot              Reason = "bot"               // standard price, or fallback; never enrolled
	ReasonDisabled         Reason = "disabled"          // kill switch, or no valid secret key
	ReasonNoConfig         Reason = "no_config"         // no usable signed config; sets the pin cookie
	ReasonPinnedFallback   Reason = "pinned_fallback"   // this browser got a fallback during an outage
	ReasonUnknownProduct   Reason = "unknown_product"   // the config has no such product
	ReasonCurrencyMismatch Reason = "currency_mismatch" // the config's currency differs from the guard
	ReasonOutOfBounds      Reason = "out_of_bounds"     // a price is outside the product's signed range
)

// Quote is the price to show and charge for one product. Country, ExperimentID, VariantID,
// VisitorID and CheckoutURL are empty when unset and encode as JSON null.
type Quote struct {
	ProductKey     string            `json:"productKey"`
	Price          int64             `json:"price"`
	Currency       string            `json:"currency"` // the product's currency, or the region's when Regional
	Country        string            `json:"country"`  // the country whose price applies (ISO 3166-1 alpha-2), when known: the visitor's, or a dearer one they were seen in lately (VPN guard)
	Regional       bool              `json:"regional"` // Price is the regional price of Country
	IsFallback     bool              `json:"isFallback"`
	Reason         Reason            `json:"reason"`
	ExperimentID   string            `json:"experimentId"`
	VariantID      string            `json:"variantId"`
	VisitorID      string            `json:"visitorId"`
	Metadata       map[string]string `json:"metadata"`
	SetCookie      []string          `json:"setCookie"`
	CheckoutURL    string            `json:"checkoutUrl"`
	ExposureLogged bool              `json:"exposureLogged"`
}

// HeaderWriter is implemented by http.ResponseWriter and the response writers of common
// frameworks.
type HeaderWriter interface {
	Header() http.Header
}

// WriteCookies adds every SetCookie value as a Set-Cookie header on w.
func (q *Quote) WriteCookies(w HeaderWriter) {
	if q == nil || w == nil || len(q.SetCookie) == 0 {
		return
	}
	h := w.Header()
	for _, c := range q.SetCookie {
		h.Add("Set-Cookie", c)
	}
}

// MarshalJSON implements json.Marshaler with the field shapes of the other Leverprice SDKs.
func (q Quote) MarshalJSON() ([]byte, error) {
	type wire struct {
		ProductKey     string            `json:"productKey"`
		Price          int64             `json:"price"`
		Currency       string            `json:"currency"`
		Country        *string           `json:"country"`
		Regional       bool              `json:"regional"`
		IsFallback     bool              `json:"isFallback"`
		Reason         Reason            `json:"reason"`
		ExperimentID   *string           `json:"experimentId"`
		VariantID      *string           `json:"variantId"`
		VisitorID      *string           `json:"visitorId"`
		Metadata       map[string]string `json:"metadata"`
		SetCookie      []string          `json:"setCookie"`
		CheckoutURL    *string           `json:"checkoutUrl"`
		ExposureLogged bool              `json:"exposureLogged"`
	}
	w := wire{
		ProductKey:     q.ProductKey,
		Price:          q.Price,
		Currency:       q.Currency,
		Country:        nullable(q.Country),
		Regional:       q.Regional,
		IsFallback:     q.IsFallback,
		Reason:         q.Reason,
		ExperimentID:   nullable(q.ExperimentID),
		VariantID:      nullable(q.VariantID),
		VisitorID:      nullable(q.VisitorID),
		Metadata:       q.Metadata,
		SetCookie:      q.SetCookie,
		CheckoutURL:    nullable(q.CheckoutURL),
		ExposureLogged: q.ExposureLogged,
	}
	if w.Metadata == nil {
		w.Metadata = map[string]string{}
	}
	if w.SetCookie == nil {
		w.SetCookie = []string{}
	}
	return json.Marshal(w)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fallbackQuote(productKey string, g guard, reason Reason, setCookie ...string) *Quote {
	if setCookie == nil {
		setCookie = []string{}
	}
	return &Quote{
		ProductKey: productKey,
		Price:      g.fallbackPrice,
		Currency:   g.currency,
		IsFallback: true,
		Reason:     reason,
		Metadata:   map[string]string{},
		SetCookie:  setCookie,
	}
}

func priceQuote(productKey string, g guard, price int64, reason Reason, checkoutURL string) *Quote {
	q := fallbackQuote(productKey, g, reason)
	q.Price = price
	q.IsFallback = false
	q.CheckoutURL = checkoutURL
	return q
}

// regionalQuote carries no checkout URL: hosted checkout links are made for the standard prices.
func regionalQuote(productKey string, g guard, r *region) *Quote {
	q := fallbackQuote(productKey, g, ReasonBasePrice)
	q.Price = r.price
	q.Currency = r.currency
	q.Regional = true
	q.IsFallback = false
	return q
}
