// Package httpx is the shared HTTP plumbing for the hosted provider adapters.
//
// Two decisions here affect what the benchmarks measure, so they are made once
// and shared rather than repeated per adapter:
//
//  1. A 429 is surfaced as a typed error instead of being retried silently.
//     A cluster benchmark that quietly absorbs rate limiting reports a
//     throughput ceiling that is really a quota, and the two must not be
//     confused in a result file.
//  2. The transport keeps a large connection pool. With the default pool, a
//     burst of a thousand creates queues on the client's own connections, and
//     the benchmark then measures the runner rather than the provider.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a small JSON-over-HTTP client for one provider's API.
type Client struct {
	BaseURL string
	// Header is applied to every request, and carries the adapter's auth.
	Header http.Header
	HTTP   *http.Client
}

// RateLimited is returned for HTTP 429. Callers record it rather than retry,
// so that quota shows up as quota.
type RateLimited struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimited) Error() string {
	return fmt.Sprintf("rate limited by provider (retry after %s): %s", e.RetryAfter, e.Body)
}

// StatusError is any other non-2xx response.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Code, truncate(e.Body, 500))
}

// New builds a client with a transport sized for burst workloads.
func New(baseURL string, header http.Header, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 4096
	tr.MaxIdleConnsPerHost = 2048
	tr.MaxConnsPerHost = 0
	tr.IdleConnTimeout = 90 * time.Second
	if header == nil {
		header = http.Header{}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Header:  header,
		HTTP:    &http.Client{Timeout: timeout, Transport: tr},
	}
}

// Do sends a request and decodes a JSON response into out, which may be nil.
//
// extra headers are applied after the client-wide ones, so a per-request
// Idempotency-Key can be added without mutating shared state.
func (c *Client) Do(ctx context.Context, method, path string, body, out any, extra http.Header) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}

	url := path
	if !strings.HasPrefix(path, "http") {
		url = c.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, vs := range extra {
		req.Header[k] = vs
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{RetryAfter: retryAfter(resp.Header.Get("Retry-After")), Body: truncate(string(raw), 500)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w: %s", method, path, err, truncate(string(raw), 300))
	}
	return nil
}

// PollUntil calls check until it reports done, the context ends, or the
// deadline passes. Adapters use it for readiness, which is why create latency
// always means create-to-ready.
func PollUntil(ctx context.Context, interval, limit time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		done, err := check(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for readiness", limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if d, err := time.ParseDuration(h + "s"); err == nil {
		return d
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
