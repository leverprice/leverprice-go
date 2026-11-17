package leverprice

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	protocolVersion     = 2
	configVersionHeader = "Leverprice-Config-Version"
	maxEventsPerRequest = 500
	maxMinorUnits       = 10_000_000_000
)

type keyset struct {
	version   int64
	expiresAt float64
	expires   bool
	keys      []signingKey
}

type variant struct {
	id          string
	price       int64
	weight      float64
	checkoutURL string
}

type experiment struct {
	id       string
	salt     string
	variants []variant
}

type configProduct struct {
	currency    string
	price       int64
	minPrice    int64
	maxPrice    int64
	checkoutURL string
	experiment  *experiment
}

type config struct {
	projectID       string
	version         int64
	expiresAt       float64
	assignmentKey   string
	key             []byte
	keyFingerprints []string
	products        map[string]*configProduct
}

// parseConfig drops a malformed product but rejects the config for anything else malformed.
func parseConfig(payload string) (*config, []string, error) {
	raw, err := decodeJSONPayload(payload)
	if err != nil {
		return nil, nil, errors.New("leverprice config: payload is not base64url JSON")
	}
	obj, ok := jsonObject(raw)
	if !ok {
		return nil, nil, errors.New("leverprice config: payload is not an object")
	}
	if schema, ok := jsonNumber(obj["schema"]); !ok || schema != protocolVersion {
		return nil, nil, fmt.Errorf("leverprice config: unsupported schema %s (this SDK speaks schema %d)",
			jsonText(obj["schema"]), protocolVersion)
	}
	list, ok := jsonArray(obj["keyFingerprints"])
	if !ok {
		return nil, nil, errors.New("leverprice config: bad keyFingerprints")
	}
	fingerprints := make([]string, 0, len(list))
	for _, item := range list {
		f, ok := jsonString(item)
		if !ok || !isLowerHex(f, 16) {
			return nil, nil, errors.New("leverprice config: bad keyFingerprints")
		}
		fingerprints = append(fingerprints, f)
	}
	projectID, ok := jsonString(obj["projectId"])
	if !ok || projectID == "" {
		return nil, nil, errors.New("leverprice config: no projectId")
	}
	version, ok := jsonSafeInt(obj["version"])
	if !ok || version < 0 {
		return nil, nil, errors.New("leverprice config: bad version")
	}
	expiresAt, ok := jsonNumber(obj["expiresAt"])
	if !ok || !isFinite(expiresAt) {
		return nil, nil, errors.New("leverprice config: bad expiresAt")
	}
	assignmentKey, _ := jsonString(obj["assignmentKey"])
	key, err := fromB64url(assignmentKey)
	if err != nil || len(key) < 32 {
		return nil, nil, errors.New("leverprice config: assignmentKey must be at least 32 bytes of base64url")
	}
	members, ok := jsonOrderedObject(obj["products"])
	if !ok {
		return nil, nil, errors.New("leverprice config: no products")
	}
	products := make(map[string]*configProduct, len(members))
	dropped := []string{}
	for _, m := range members {
		if p := parseProduct(m.value); p != nil {
			products[m.key] = p
		} else {
			dropped = append(dropped, m.key)
		}
	}
	return &config{
		projectID:       projectID,
		version:         version,
		expiresAt:       expiresAt,
		assignmentKey:   assignmentKey,
		key:             key,
		keyFingerprints: fingerprints,
		products:        products,
	}, dropped, nil
}

func parseProduct(raw json.RawMessage) *configProduct {
	p, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	currency, ok := jsonString(p["currency"])
	if !ok || !isCurrencyCode(currency) {
		return nil
	}
	price, ok1 := jsonSafeInt(p["price"])
	minPrice, ok2 := jsonSafeInt(p["minPrice"])
	maxPrice, ok3 := jsonSafeInt(p["maxPrice"])
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	out := &configProduct{currency: asciiLower(currency), price: price, minPrice: minPrice, maxPrice: maxPrice}
	if e, present := p["experiment"]; present && !isJSONNull(e) {
		if out.experiment = parseExperiment(e); out.experiment == nil {
			return nil
		}
	}
	url, _ := jsonString(p["checkoutUrl"])
	out.checkoutURL = httpsURL(url)
	return out
}

func parseExperiment(raw json.RawMessage) *experiment {
	e, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	id, ok1 := jsonString(e["id"])
	salt, ok2 := jsonString(e["salt"])
	list, ok3 := jsonArray(e["variants"])
	if !ok1 || !isID(id) || !ok2 || !ok3 {
		return nil
	}
	exp := &experiment{id: id, salt: salt, variants: make([]variant, 0, len(list))}
	for _, item := range list {
		v, ok := jsonObject(item)
		if !ok {
			return nil
		}
		vid, ok1 := jsonString(v["id"])
		price, ok2 := jsonSafeInt(v["price"])
		weight, ok3 := jsonSafeInt(v["weight"])
		if !ok1 || !isID(vid) || !ok2 || !ok3 || weight < 0 {
			return nil
		}
		url, _ := jsonString(v["checkoutUrl"])
		exp.variants = append(exp.variants, variant{id: vid, price: price, weight: float64(weight), checkoutURL: httpsURL(url)})
	}
	return exp
}

func jsonText(raw json.RawMessage) string {
	if raw == nil {
		return "undefined"
	}
	if s, ok := jsonString(raw); ok {
		return s
	}
	return string(raw)
}

type wireEvent interface {
	eventType() string
}

type exposureEvent struct {
	Type         string `json:"type"`
	ExperimentID string `json:"experimentId"`
	VariantID    string `json:"variantId"`
	VisitorID    string `json:"visitorId"`
	Timestamp    int64  `json:"timestamp"`
}

type wireAttribution struct {
	ExperimentID string  `json:"experimentId"`
	VariantID    string  `json:"variantId"`
	VisitorID    string  `json:"visitorId"`
	Signature    *string `json:"signature"`
}

type purchaseEvent struct {
	Type        string           `json:"type"`
	Provider    Provider         `json:"provider"`
	OrderID     string           `json:"orderId"`
	Amount      int64            `json:"amount"`
	Currency    string           `json:"currency"`
	Timestamp   int64            `json:"timestamp"`
	Attribution *wireAttribution `json:"attribution"`
}

type refundEvent struct {
	Type          string   `json:"type"`
	Provider      Provider `json:"provider"`
	OrderID       string   `json:"orderId"`
	Currency      string   `json:"currency"`
	Timestamp     int64    `json:"timestamp"`
	RefundID      *string  `json:"refundId,omitempty"`
	Amount        *int64   `json:"amount,omitempty"`
	RefundedTotal *int64   `json:"refundedTotal,omitempty"`
}

func (exposureEvent) eventType() string { return "exposure" }
func (purchaseEvent) eventType() string { return "purchase" }
func (refundEvent) eventType() string   { return "refund" }

type eventsBody struct {
	Events []wireEvent `json:"events"`
	SDK    sdkInfo     `json:"sdk"`
}

type sdkInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func isMinorUnits(n int64) bool { return n >= 0 && n <= maxMinorUnits }

func isText(s string) bool {
	n := utf16Len(s)
	return n >= 1 && n <= 255
}

func validTimestamp(t int64) bool { return t > 0 && t <= maxSafeInteger }

// eventProblem mirrors the server's schema, which rejects a whole batch for one invalid event.
func eventProblem(e wireEvent) string {
	switch e := e.(type) {
	case exposureEvent:
		if !validTimestamp(e.Timestamp) {
			return "timestamp must be a positive integer (ms)"
		}
		if !isID(e.ExperimentID) || !isID(e.VariantID) || !isID(e.VisitorID) {
			return "invalid experiment/variant/visitor id"
		}
	case purchaseEvent:
		if !validTimestamp(e.Timestamp) {
			return "timestamp must be a positive integer (ms)"
		}
		if p := moneyProblem(e.Provider, e.OrderID, e.Currency); p != "" {
			return p
		}
		if !isMinorUnits(e.Amount) {
			return "amount must be a non-negative integer in minor units"
		}
		if a := e.Attribution; a != nil && (!isID(a.ExperimentID) || !isID(a.VariantID) || !isID(a.VisitorID) ||
			a.Signature != nil && !isLowerHex(*a.Signature, 16)) {
			return "invalid attribution"
		}
	case refundEvent:
		if !validTimestamp(e.Timestamp) {
			return "timestamp must be a positive integer (ms)"
		}
		if p := moneyProblem(e.Provider, e.OrderID, e.Currency); p != "" {
			return p
		}
		if e.RefundID != nil && !isText(*e.RefundID) {
			return "refundId must be a string of 1–255 characters"
		}
		if e.Amount != nil && !isMinorUnits(*e.Amount) {
			return "amount must be a non-negative integer"
		}
		if e.RefundedTotal != nil && !isMinorUnits(*e.RefundedTotal) {
			return "refundedTotal must be a non-negative integer"
		}
		if (e.RefundID == nil || e.Amount == nil) && e.RefundedTotal == nil {
			return "refund needs refundId + amount, or refundedTotal"
		}
	default:
		return "unknown event type"
	}
	return ""
}

func moneyProblem(provider Provider, orderID, currency string) string {
	switch provider {
	case ProviderStripe, ProviderPolar, ProviderLemonSqueezy, ProviderDodo, ProviderOther:
	default:
		return fmt.Sprintf("unknown provider %q", string(provider))
	}
	if !isText(orderID) {
		return "orderId must be a string of 1–255 characters"
	}
	if !isLowerCurrencyCode(currency) {
		return "currency must be a 3-letter ISO 4217 code"
	}
	return ""
}
