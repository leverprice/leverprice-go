package leverprice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r *apiResponse) ok() bool { return r.status >= 200 && r.status <= 299 }

// request enforces the timeout itself, so an HTTPDoer that ignores its context cannot block.
func (c *Client) request(ctx context.Context, method, path string, headers map[string]string, body []byte) (*apiResponse, error) {
	timedOut := fmt.Errorf("leverprice: %s %s timed out after %d ms", method, path, requestTimeout.Milliseconds())
	ctx, cancel := context.WithTimeoutCause(ctx, requestTimeout, timedOut)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("leverprice: %s %s: %w", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("User-Agent", "leverprice-sdk-go/"+Version)

	type result struct {
		res *apiResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("leverprice: %s %s: HTTP client panicked: %v", method, path, r)}
			}
		}()
		res, err := c.do(req)
		if err != nil {
			err = fmt.Errorf("leverprice: %s %s: %w", method, path, err)
		}
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return r.res, r.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (c *Client) do(req *http.Request) (*apiResponse, error) {
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("no response")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBodyLength+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyLength {
		return nil, errors.New("response body too large")
	}
	return &apiResponse{status: res.StatusCode, header: res.Header, body: data}, nil
}
