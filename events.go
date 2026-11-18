package leverprice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Provider is a payment provider.
type Provider string

// Payment providers.
const (
	ProviderStripe       Provider = "stripe"
	ProviderPolar        Provider = "polar"
	ProviderLemonSqueezy Provider = "lemonsqueezy"
	ProviderDodo         Provider = "dodo"
	ProviderPaddle       Provider = "paddle"
	ProviderCreem        Provider = "creem"
	ProviderOther        Provider = "other"
)

// Purchase is a completed payment. Amounts are in minor units, after discounts, excluding tax.
type Purchase struct {
	Provider  Provider
	OrderID   string // as the provider's webhook reports it, e.g. the Stripe PaymentIntent id
	Amount    int64
	Currency  string
	Metadata  map[string]string // the checkout's metadata; pp_exp, pp_var, pp_vid and pp_sig are read from it
	Timestamp int64             // milliseconds or Unix seconds since the epoch; 0 means now
}

// Refund is one refund (RefundID and Amount) or an order's cumulative refunded total
// (RefundedTotal, which takes precedence when set). Amounts are in minor units.
type Refund struct {
	Provider      Provider
	OrderID       string
	Currency      string
	RefundID      string
	Amount        int64
	RefundedTotal *int64
	Timestamp     int64 // milliseconds or Unix seconds since the epoch; 0 means now
}

// TrackPurchase records a purchase and sends it at once, waiting at most until ctx is done.
func (c *Client) TrackPurchase(ctx context.Context, p Purchase) {
	if c.off() {
		return
	}
	func() {
		defer c.recoverReport()
		id := func(k string) string {
			if v := p.Metadata[k]; isID(v) {
				return v
			}
			return ""
		}
		var attribution *wireAttribution
		if exp, vr, vid := id("pp_exp"), id("pp_var"), id("pp_vid"); exp != "" && vr != "" && vid != "" {
			attribution = &wireAttribution{ExperimentID: exp, VariantID: vr, VisitorID: vid}
			if sig := p.Metadata["pp_sig"]; isLowerHex(sig, 16) {
				attribution.Signature = &sig
			}
		}
		c.enqueue(purchaseEvent{
			Type:        "purchase",
			Provider:    p.Provider,
			OrderID:     p.OrderID,
			Amount:      p.Amount,
			Currency:    strings.ToLower(p.Currency),
			Timestamp:   c.timestampOf(p.Timestamp),
			Attribution: attribution,
		})
	}()
	c.Flush(ctx)
}

// TrackRefund records a refund and sends it at once, waiting at most until ctx is done.
func (c *Client) TrackRefund(ctx context.Context, r Refund) {
	if c.off() {
		return
	}
	func() {
		defer c.recoverReport()
		e := refundEvent{
			Type:      "refund",
			Provider:  r.Provider,
			OrderID:   r.OrderID,
			Currency:  strings.ToLower(r.Currency),
			Timestamp: c.timestampOf(r.Timestamp),
		}
		if r.RefundedTotal != nil {
			total := *r.RefundedTotal
			e.RefundedTotal = &total
		} else {
			refundID, amount := r.RefundID, r.Amount
			e.RefundID, e.Amount = &refundID, &amount
		}
		c.enqueue(e)
	}()
	c.Flush(ctx)
}

func (c *Client) timestampOf(t int64) int64 {
	switch {
	case t == 0:
		return c.nowMs()
	case t < 1e12:
		return t * 1000
	}
	return t
}

func (c *Client) recoverReport() {
	if r := recover(); r != nil {
		c.report(fmt.Errorf("leverprice: internal error: %v", r))
	}
}

func (c *Client) wasExposed(experimentID, visitorID string) bool {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	_, ok := c.exposed[experimentID+":"+visitorID]
	return ok
}

func (c *Client) recordExposure(experimentID, variantID, visitorID, country string) bool {
	k := experimentID + ":" + visitorID
	c.qmu.Lock()
	if _, ok := c.exposed[k]; ok {
		c.qmu.Unlock()
		return false
	}
	if len(c.exposed) >= maxDedupe {
		clear(c.exposed)
	}
	c.exposed[k] = struct{}{}
	c.qmu.Unlock()
	return c.enqueue(exposureEvent{
		Type:         "exposure",
		ExperimentID: experimentID,
		VariantID:    variantID,
		VisitorID:    visitorID,
		Timestamp:    c.nowMs(),
		Country:      country,
	})
}

func (c *Client) enqueue(e wireEvent) bool {
	if problem := eventProblem(e); problem != "" {
		c.report(fmt.Errorf("leverprice: dropped invalid %s event: %s", e.eventType(), problem))
		return false
	}
	c.qmu.Lock()
	c.queue = append(c.queue, e)
	dropped := c.trimQueueLocked()
	flushNow := len(c.queue) >= autoFlushAt
	if !flushNow {
		c.armTimerLocked()
	}
	c.qmu.Unlock()
	c.reportDropped(dropped)
	if flushNow {
		c.startFlush()
	}
	return true
}

func (c *Client) armTimerLocked() {
	if c.timer != nil || c.closed.Load() {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(c.flushInterval, func() {
		c.qmu.Lock()
		current := c.timer == t
		if current {
			c.timer = nil
		}
		c.qmu.Unlock()
		if current {
			c.startFlush()
		}
	})
	c.timer = t
}

// trimQueueLocked bounds memory: exposures are dropped first, money events only as a last resort.
func (c *Client) trimQueueLocked() []wireEvent {
	var dropped []wireEvent
	for len(c.queue) > maxQueue {
		i := 0
		for j, e := range c.queue {
			if _, ok := e.(exposureEvent); ok {
				i = j
				break
			}
		}
		e := c.queue[i]
		c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
		if _, ok := e.(exposureEvent); !ok {
			dropped = append(dropped, e)
		}
	}
	return dropped
}

func (c *Client) reportDropped(dropped []wireEvent) {
	for _, e := range dropped {
		orderID := ""
		switch e := e.(type) {
		case purchaseEvent:
			orderID = e.OrderID
		case refundEvent:
			orderID = e.OrderID
		}
		c.report(fmt.Errorf("leverprice: event queue full, dropped %s %s", e.eventType(), orderID))
	}
}

// Flush sends the queued events and waits until they are sent or ctx is done; in the latter case
// sending continues in the background.
func (c *Client) Flush(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	done := c.startFlush()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *Client) startFlush() <-chan struct{} {
	c.qmu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	previous := c.flushTail
	done := make(chan struct{})
	c.flushTail = done
	c.qmu.Unlock()
	go func() {
		defer close(done)
		if previous != nil {
			<-previous
		}
		c.drain()
	}()
	return done
}

func (c *Client) drain() {
	defer c.recoverReport()
	if c.off() {
		return
	}
	c.qmu.Lock()
	n := (len(c.queue) + maxEventsPerRequest - 1) / maxEventsPerRequest
	c.qmu.Unlock()
	for ; n > 0; n-- {
		c.qmu.Lock()
		if len(c.queue) == 0 {
			c.qmu.Unlock()
			return
		}
		k := min(maxEventsPerRequest, len(c.queue))
		batch := append([]wireEvent(nil), c.queue[:k]...)
		c.queue = append([]wireEvent(nil), c.queue[k:]...)
		c.qmu.Unlock()
		if !c.send(batch) {
			return
		}
	}
}

// requeue keeps the money events of an undelivered batch; exposures are best-effort.
func (c *Client) requeue(events []wireEvent) {
	money := make([]wireEvent, 0, len(events))
	for _, e := range events {
		if _, ok := e.(exposureEvent); !ok {
			money = append(money, e)
		}
	}
	if len(money) == 0 {
		return
	}
	c.qmu.Lock()
	c.queue = append(money, c.queue...)
	dropped := c.trimQueueLocked()
	c.qmu.Unlock()
	c.reportDropped(dropped)
}

func (c *Client) send(events []wireEvent) bool {
	body, err := json.Marshal(eventsBody{Events: events, SDK: sdkInfo{Name: "leverprice-go", Version: Version}})
	if err != nil {
		c.report(fmt.Errorf("leverprice: cannot encode events: %w", err))
		return true
	}
	var lastErr error
	for attempt := 0; attempt < sendAttempts && !c.off(); attempt++ {
		if attempt > 0 {
			c.sleep(250 * time.Millisecond << (2 * (attempt - 1)))
		}
		res, err := c.request(context.Background(), http.MethodPost, "/v1/events",
			map[string]string{"Content-Type": "application/json"}, body)
		if err != nil {
			lastErr = err
			continue
		}
		c.noteServerVersion(res.header)
		if res.ok() {
			return true
		}
		// Other 4xx responses are permanent: retrying cannot help, so the batch counts as sent.
		if res.status < 500 && res.status != http.StatusRequestTimeout && res.status != http.StatusTooManyRequests {
			c.report(fmt.Errorf("leverprice events rejected: HTTP %d %s", res.status, truncateUTF16(string(res.body), 300)))
			return true
		}
		lastErr = fmt.Errorf("leverprice events: HTTP %d", res.status)
	}
	c.requeue(events)
	c.report(lastErr)
	return false
}

// Close flushes queued events, waiting at most until ctx is done, and stops all background work.
// The Client keeps serving prices from the config it holds; it is safe to call Close twice.
func (c *Client) Close(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.closed.Store(true)
	c.cancelFetches()
	c.Flush(ctx)
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f != nil {
		select {
		case <-f.done:
		case <-ctx.Done():
		}
	}
}
