package leverprice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type flight struct {
	done      chan struct{}
	startedAt int64
}

func (c *Client) usableLocked(now int64) *config {
	cfg := c.config
	if cfg == nil || float64(now) >= cfg.expiresAt || now-c.fetchedAt >= c.maxStaleMs {
		return nil
	}
	if c.signingKey != nil && !c.trust.isTrusted(*c.signingKey) {
		return nil
	}
	return cfg
}

// currentConfig waits for a cold-start fetch at most Timeout after that fetch started, so
// concurrent requests all give up at the same moment.
func (c *Client) currentConfig(ctx context.Context) *config {
	now := c.nowMs()
	c.mu.Lock()
	ready := c.usableLocked(now)
	c.kickLocked(now)
	if ready != nil {
		c.mu.Unlock()
		return ready
	}
	f := c.liveFlightLocked(now)
	c.mu.Unlock()
	if f == nil {
		return nil
	}
	if remaining := c.timeoutMs - (now - f.startedAt); remaining > 0 {
		waitAtMost(ctx, f.done, time.Duration(remaining)*time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usableLocked(c.nowMs())
}

func (c *Client) kickLocked(now int64) {
	if c.liveFlightLocked(now) != nil {
		return
	}
	if c.serverVersion > c.loadedVersionLocked() && now >= c.catchUpAt {
		c.catchUpAt = now + catchUpGapMs
		c.startFetchLocked(now, false, true)
	} else if c.usableLocked(now) == nil || now-c.fetchedAt >= c.refreshMs {
		c.startFetchLocked(now, false, false)
	}
}

func (c *Client) loadedVersionLocked() int64 {
	if c.config == nil {
		return 0
	}
	return c.config.version
}

// liveFlightLocked treats a fetch older than any request can take as lost.
func (c *Client) liveFlightLocked(now int64) *flight {
	if c.flight != nil && now-c.flight.startedAt < stallMs {
		return c.flight
	}
	return nil
}

func (c *Client) startFetchLocked(now int64, force, unconditional bool) *flight {
	if f := c.liveFlightLocked(now); f != nil {
		return f
	}
	if c.closed.Load() || (!force && now < c.retryAt) {
		return nil
	}
	f := &flight{done: make(chan struct{}), startedAt: now}
	c.flight = f
	go func() {
		defer func() {
			c.mu.Lock()
			if c.flight == f {
				c.flight = nil
			}
			c.mu.Unlock()
			close(f.done)
		}()
		c.loadConfig(unconditional)
	}()
	return f
}

func (c *Client) loadConfig(unconditional bool) {
	note, err := c.fetchConfig(unconditional)
	if note != nil {
		c.report(note)
	}
	if err == nil {
		return
	}
	c.mu.Lock()
	c.failures++
	backoff := int64(maxBackoffMs)
	if c.failures < 6 {
		backoff = min(maxBackoffMs, int64(500)<<c.failures)
	}
	c.retryAt = c.nowMs() + backoff
	c.mu.Unlock()
	if !c.closed.Load() {
		c.report(err)
	}
}

func (c *Client) fetchConfig(unconditional bool) (note, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("leverprice: internal error: %v", r)
		}
	}()
	c.mu.Lock()
	etag := ""
	// Revalidate only a config that is still usable; an expired or revoked one needs a new body.
	if !unconditional && c.etag != "" && c.usableLocked(c.nowMs()) != nil {
		etag = c.etag
	}
	c.mu.Unlock()
	headers := map[string]string{}
	if etag != "" {
		headers["If-None-Match"] = etag
	}
	res, err := c.request(c.fetchCtx, http.MethodGet, "/v1/config", headers, nil)
	if err != nil {
		return nil, err
	}
	if res.status == http.StatusNotModified && etag != "" {
		c.mu.Lock()
		c.fetchedAt = c.nowMs()
		c.failures, c.retryAt = 0, 0
		c.mu.Unlock()
	} else {
		if !res.ok() {
			return nil, fmt.Errorf("leverprice config: HTTP %d%s", res.status, errorCode(res.body))
		}
		body := stripBOM(res.body)
		if !json.Valid(body) {
			return nil, errors.New("leverprice config: response is not JSON")
		}
		if note, err = c.storeConfig(body, res.header.Get("ETag")); err != nil {
			return nil, err
		}
	}
	c.noteServerVersion(res.header)
	return note, nil
}

func (c *Client) storeConfig(body []byte, etag string) (note, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, note, err = c.acceptLocked(body, c.nowMs()); err != nil {
		return nil, err
	}
	c.etag = etag
	c.fetchedAt = c.nowMs()
	c.failures, c.retryAt = 0, 0
	return note, nil
}

func (c *Client) acceptLocked(envelope json.RawMessage, now int64) (dropped []string, note, err error) {
	cfg, key, dropped, err := c.trust.verify(envelope, now)
	if err != nil {
		return nil, nil, err
	}
	// A validly signed config of another project (e.g. from a compromised cache) is refused even
	// on the very first load: the config names the API keys it was issued for.
	issued := false
	for _, f := range cfg.keyFingerprints {
		issued = issued || f == c.fingerprint
	}
	if !issued {
		return nil, nil, errors.New("leverprice config: not issued for this API key; refusing")
	}
	current := c.config
	if current != nil && cfg.projectID != current.projectID {
		return nil, nil, fmt.Errorf("leverprice config: project changed from %s to %s; refusing", current.projectID, cfg.projectID)
	}
	if current != nil && cfg.version < current.version {
		return nil, nil, fmt.Errorf("leverprice config: refusing to roll back from version %d to %d", current.version, cfg.version)
	}
	if len(dropped) > 0 && (current == nil || cfg.version != current.version) {
		note = fmt.Errorf("leverprice config v%d: ignored malformed products %s", cfg.version, strings.Join(dropped, ", "))
	}
	c.config = cfg
	c.signingKey = &key
	return dropped, note, nil
}

func (c *Client) noteServerVersion(h http.Header) {
	v, ok := parseVersion(h.Get(configVersionHeader))
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v <= c.serverVersion {
		return
	}
	c.serverVersion = v
	if v > c.loadedVersionLocked() {
		c.kickLocked(c.nowMs())
	}
}

func parseVersion(s string) (int64, bool) {
	s = jsTrim(s)
	if s == "" || strings.ContainsAny(s, "xX_") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !isSafeInteger(f) || f < 0 {
		return 0, false
	}
	return int64(f), true
}

func errorCode(body []byte) string {
	obj, ok := jsonObject(stripBOM(body))
	if !ok {
		return ""
	}
	e, ok := jsonObject(obj["error"])
	if !ok {
		return ""
	}
	if code, ok := jsonString(e["code"]); ok {
		return " (" + code + ")"
	}
	return ""
}

func waitAtMost(ctx context.Context, done <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// Refresh fetches the config now and waits for it, at most until ctx is done.
func (c *Client) Refresh(ctx context.Context) {
	if c.off() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stall := time.Duration(stallMs) * time.Millisecond
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f != nil {
		waitAtMost(ctx, f.done, stall)
	}
	c.mu.Lock()
	f = c.startFetchLocked(c.nowMs(), true, false)
	c.mu.Unlock()
	if f != nil {
		waitAtMost(ctx, f.done, stall)
	}
}

// Preload fetches the config unless a usable one is loaded and waits for it, at most until ctx
// is done. Call it at startup so the first visitors do not wait.
func (c *Client) Preload(ctx context.Context) {
	if c.off() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.usableLocked(c.nowMs()) != nil {
		c.mu.Unlock()
		return
	}
	f := c.startFetchLocked(c.nowMs(), true, false)
	c.mu.Unlock()
	if f != nil {
		waitAtMost(ctx, f.done, time.Duration(stallMs)*time.Millisecond)
	}
	c.mu.Lock()
	// A failed preload must not delay the first request's own attempt.
	if c.usableLocked(c.nowMs()) == nil {
		c.retryAt = 0
	}
	c.mu.Unlock()
}
