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
	maxRegions          = 300
	maxSplits           = 300
	maxScope            = 300
	maxCountryHold      = 90 * 86_400
	standardLevel       = 10_000 // the level of a country without a regional price
)

// Why a config was refused, sent back to the server in Leverprice-Config-Error.
const (
	codeMalformed    = "malformed"
	codeUntrusted    = "untrusted"
	codeBadSignature = "bad_signature"
	codeExpired      = "expired"
	codeWrongKey     = "wrong_key"
	codeWrongProject = "wrong_project"
	codeRollback     = "rollback"
)

// configError is a refused config and the code of the refusal.
type configError struct {
	code string
	err  error
}

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

// coded gives err the code of a refusal unless it already has one.
func coded(code string, err error) error {
	var ce *configError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return &configError{code: code, err: err}
}

// refusalCode returns the code of a refused config, or "" for any other error.
func refusalCode(err error) string {
	var ce *configError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ""
}

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
	regional    []int64
}

// experiment is the one test of a product. A nil scope opens it to every visitor; otherwise it
// runs only for visitors from those countries. splitOf maps a country to the weights its visitors
// are split by, one per variant, in place of the variants' own.
type experiment struct {
	id       string
	salt     string
	variants []variant
	scope    map[string]struct{}
	splitOf  map[string][]float64
}

// region is one regional price of a product; index also selects each variant's regional price.
// level is the price as basis points of the standard price (10000 = the same), to compare
// countries.
type region struct {
	index    int
	currency string
	price    int64
	level    int64
}

type configProduct struct {
	currency    string
	price       int64
	minPrice    int64
	maxPrice    int64
	checkoutURL string
	regions     []region
	regionOf    map[string]int
	experiment  *experiment
}

type config struct {
	projectID       string
	version         int64
	expiresAt       float64
	assignmentKey   string
	key             []byte
	keyFingerprints []string
	countryHeader   string
	countryHold     int64 // seconds a visitor keeps the price of a dearer country they were seen in; 0: off
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
	countryHeader, _ := jsonString(obj["countryHeader"])
	if !isToken(countryHeader) || visitorSetHeader(countryHeader) {
		countryHeader = ""
	}
	countryHold, ok := jsonSafeInt(obj["countryHold"])
	if !ok || countryHold <= 0 || countryHold > maxCountryHold {
		countryHold = 0
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
		countryHeader:   asciiLower(countryHeader),
		countryHold:     countryHold,
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
	if r, present := p["regions"]; present && !isJSONNull(r) {
		if out.regions, out.regionOf, ok = parseRegions(r); !ok {
			return nil
		}
	}
	if e, present := p["experiment"]; present && !isJSONNull(e) {
		if out.experiment = parseExperiment(e, len(out.regions), false); out.experiment == nil {
			return nil
		}
	}
	if e, present := p["scopedExperiment"]; present && !isJSONNull(e) {
		if out.experiment != nil {
			return nil
		}
		if out.experiment = parseExperiment(e, len(out.regions), true); out.experiment == nil {
			return nil
		}
	}
	url, _ := jsonString(p["checkoutUrl"])
	out.checkoutURL = httpsURL(url)
	return out
}

// parseRegions rejects a country listed twice: a visitor has at most one regional price.
func parseRegions(raw json.RawMessage) ([]region, map[string]int, bool) {
	list, ok := jsonArray(raw)
	if !ok || len(list) > maxRegions {
		return nil, nil, false
	}
	regions := make([]region, 0, len(list))
	regionOf := make(map[string]int)
	for i, item := range list {
		r, ok := jsonObject(item)
		if !ok {
			return nil, nil, false
		}
		countries, ok := parseCountries(r["countries"])
		if !ok {
			return nil, nil, false
		}
		for _, country := range countries {
			if _, listed := regionOf[country]; listed {
				return nil, nil, false
			}
			regionOf[country] = i
		}
		currency, ok1 := jsonString(r["currency"])
		price, ok2 := jsonSafeInt(r["price"])
		if !ok1 || !isCurrencyCode(currency) || !ok2 || price <= 0 {
			return nil, nil, false
		}
		// A level this SDK can't read compares as the standard price: it never holds a price.
		level, ok := jsonSafeInt(r["level"])
		if !ok || level <= 0 {
			level = standardLevel
		}
		regions = append(regions, region{index: i, currency: asciiLower(currency), price: price, level: level})
	}
	return regions, regionOf, true
}

// parseCountries reads a non-empty list of country codes.
func parseCountries(raw json.RawMessage) ([]string, bool) {
	list, ok := jsonArray(raw)
	if !ok || len(list) == 0 {
		return nil, false
	}
	countries := make([]string, len(list))
	for i, item := range list {
		if countries[i], ok = jsonString(item); !ok || !isCountryCode(countries[i]) {
			return nil, false
		}
	}
	return countries, true
}

// parseRegional reads a variant's prices: exactly one per region of its product, in order.
func parseRegional(raw json.RawMessage, regions int) ([]int64, bool) {
	list, ok := jsonArray(raw)
	if !ok || len(list) != regions {
		return nil, false
	}
	prices := make([]int64, len(list))
	for i, item := range list {
		price, ok := jsonSafeInt(item)
		if !ok || price <= 0 {
			return nil, false
		}
		prices[i] = price
	}
	return prices, true
}

// parseExperiment reads a test. A scoped one also lists the countries it runs in.
func parseExperiment(raw json.RawMessage, regions int, scoped bool) *experiment {
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
		weight, ok3 := parseWeight(v["weight"])
		if !ok1 || !isID(vid) || !ok2 || !ok3 {
			return nil
		}
		var regional []int64
		if regions > 0 {
			if regional, ok = parseRegional(v["regional"], regions); !ok {
				return nil
			}
		}
		url, _ := jsonString(v["checkoutUrl"])
		exp.variants = append(exp.variants, variant{
			id:          vid,
			price:       price,
			weight:      weight,
			checkoutURL: httpsURL(url),
			regional:    regional,
		})
	}
	if s, present := e["splits"]; present && !isJSONNull(s) {
		if exp.splitOf, ok = parseSplits(s, len(exp.variants)); !ok {
			return nil
		}
	}
	if scoped {
		if exp.scope, ok = parseScope(e["countries"]); !ok {
			return nil
		}
	}
	return exp
}

func parseWeight(raw json.RawMessage) (float64, bool) {
	weight, ok := jsonSafeInt(raw)
	return float64(weight), ok && weight >= 0
}

// parseSplits rejects a country listed twice: its visitors are split one way.
func parseSplits(raw json.RawMessage, variants int) (map[string][]float64, bool) {
	list, ok := jsonArray(raw)
	if !ok || len(list) > maxSplits {
		return nil, false
	}
	splitOf := make(map[string][]float64)
	for _, item := range list {
		s, ok := jsonObject(item)
		if !ok {
			return nil, false
		}
		countries, ok1 := parseCountries(s["countries"])
		weights, ok2 := parseWeights(s["weights"], variants)
		if !ok1 || !ok2 {
			return nil, false
		}
		for _, country := range countries {
			if _, listed := splitOf[country]; listed {
				return nil, false
			}
			splitOf[country] = weights
		}
	}
	return splitOf, true
}

// parseWeights reads a split's weights: exactly one per variant of its test, in order.
func parseWeights(raw json.RawMessage, variants int) ([]float64, bool) {
	list, ok := jsonArray(raw)
	if !ok || len(list) != variants {
		return nil, false
	}
	weights := make([]float64, len(list))
	for i, item := range list {
		if weights[i], ok = parseWeight(item); !ok {
			return nil, false
		}
	}
	return weights, true
}

// parseScope reads the countries a scoped test runs in, each listed once.
func parseScope(raw json.RawMessage) (map[string]struct{}, bool) {
	countries, ok := parseCountries(raw)
	if !ok || len(countries) > maxScope {
		return nil, false
	}
	set := make(map[string]struct{}, len(countries))
	for _, country := range countries {
		set[country] = struct{}{}
	}
	return set, len(set) == len(countries)
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
	Country      string `json:"country,omitempty"`
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
		if e.Country != "" && !isCountryCode(e.Country) {
			return "country must be an ISO 3166-1 alpha-2 code"
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
	case ProviderStripe, ProviderPolar, ProviderLemonSqueezy, ProviderDodo, ProviderPaddle, ProviderCreem, ProviderOther:
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
