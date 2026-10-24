// Package gateway owns the upstream exit pool and the wire-level request.
package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"oc2api/internal/catalog"
	"oc2api/internal/config"
	"oc2api/internal/zen"
)

// Exit is one network path to the upstream: the direct connection or a proxy.
// A pool exit is a rotating endpoint: it is never cooled down and every
// request opens a fresh connection, because the next connection may land on a
// different exit IP.
type Exit struct {
	Name string
	URL  *url.URL
	HTTP *http.Client
	Bun  *http.Client
	Pool bool

	mu           sync.Mutex
	blockedUntil time.Time
	reason       string
}

// Available reports whether the exit may be used now.
func (e *Exit) Available(now time.Time) bool {
	if e.Pool {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.After(e.blockedUntil)
}

// Block marks the exit unusable for d. Pool exits ignore cooldowns.
func (e *Exit) Block(d time.Duration, reason string) {
	if e.Pool {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(e.blockedUntil) {
		e.blockedUntil = until
		e.reason = reason
	}
}

// Clear releases a previous block.
func (e *Exit) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blockedUntil = time.Time{}
	e.reason = ""
}

// State is a snapshot for /healthz.
func (e *Exit) State() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := map[string]any{"name": e.Name, "blocked": time.Now().Before(e.blockedUntil), "pool": e.Pool}
	if !e.blockedUntil.IsZero() && time.Now().Before(e.blockedUntil) {
		state["until"] = e.blockedUntil.Format(time.RFC3339)
		state["reason"] = e.reason
	}
	return state
}

// Pool is the ordered exit list.
type Pool struct {
	mu     sync.Mutex
	exits  []*Exit
	cursor int
}

// NewPool builds the direct exit and one exit per proxy entry. A trailing
// "#pool" marker marks a rotating proxy pool: no cooldown, fresh connection
// per request.
func NewPool(cfg config.Config) (*Pool, error) {
	pool := &Pool{}
	if cfg.Direct {
		pool.exits = append(pool.exits, newExit("direct", nil, false))
	}
	for index, raw := range cfg.Proxies {
		entry, isPool := parsePoolMarker(raw)
		normalized := zen.NormalizeProxy(entry)
		parsed, err := url.Parse(normalized)
		if err != nil {
			return nil, fmt.Errorf("proxy %d (%s): %w", index+1, redact(raw), err)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("proxy %d (%s): missing host", index+1, redact(raw))
		}
		name := fmt.Sprintf("proxy-%d", index+1)
		if isPool {
			name += "-pool"
		}
		pool.exits = append(pool.exits, newExit(name, parsed, isPool))
	}
	if len(pool.exits) == 0 {
		return nil, fmt.Errorf("no exits configured")
	}
	return pool, nil
}

// parsePoolMarker strips a trailing "#pool" (case-insensitive) marker.
func parsePoolMarker(entry string) (string, bool) {
	trimmed := strings.TrimSpace(entry)
	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, "#pool") {
		return strings.TrimSpace(trimmed[:len(trimmed)-len("#pool")]), true
	}
	return trimmed, false
}

func newExit(name string, proxy *url.URL, isPool bool) *Exit {
	transport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return proxy, nil },
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if isPool {
		// A rotating pool must not reuse a connection: the exit IP is chosen
		// per connection, so every attempt opens a new one.
		transport.DisableKeepAlives = true
		transport.MaxIdleConnsPerHost = -1
	}
	return &Exit{
		Name: name,
		URL:  proxy,
		HTTP: &http.Client{Transport: transport},
		Bun:  newBunClient(proxy, 0),
		Pool: isPool,
	}
}

// Candidates returns the exits in rotation order, starting at the cursor.
func (p *Pool) Candidates() []*Exit {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.exits)
	out := make([]*Exit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, p.exits[(p.cursor+i)%n])
	}
	p.cursor = (p.cursor + 1) % n
	return out
}

// States reports every exit for the health endpoint.
func (p *Pool) States() []map[string]any {
	p.mu.Lock()
	exits := append([]*Exit(nil), p.exits...)
	p.mu.Unlock()
	out := make([]map[string]any, 0, len(exits))
	for _, exit := range exits {
		out = append(out, exit.State())
	}
	return out
}

func redact(entry string) string {
	if at := strings.LastIndex(entry, "@"); at >= 0 {
		return "***@" + entry[at+1:]
	}
	return entry
}

// Attempt describes one upstream call.
type Attempt struct {
	URL     string
	Headers map[string]string
	Body    []byte
	Exit    *Exit
}

// Response wraps the upstream response.
type Response struct {
	*http.Response
	Exit *Exit
}

// Do performs the attempt with the configured timeout.
func (p *Pool) Do(ctx context.Context, attempt Attempt, timeout time.Duration) (*Response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attempt.URL, strings.NewReader(string(attempt.Body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range attempt.Headers {
		req.Header.Set(name, value)
	}
	client := attempt.Exit.HTTP
	if strings.HasPrefix(attempt.URL, "https://") && attempt.Exit.Bun != nil {
		client = attempt.Exit.Bun
	}
	resp, err := client.Do(req)
	if err != nil {
		attempt.Exit.Block(15*time.Second, "transport error")
		return nil, err
	}
	return &Response{Response: resp, Exit: attempt.Exit}, nil
}

// Classify inspects a non-2xx response and blocks the exit accordingly. It
// returns a short reason, the body, and whether the model itself is gone
// (deprecated or unknown), which retrying on another exit cannot fix.
func Classify(resp *Response, blockFor, quotaFor time.Duration) (reason string, body []byte, modelGone bool) {
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	text := string(body)
	lower := strings.ToLower(text)
	switch {
	case resp.StatusCode == http.StatusGone,
		strings.Contains(lower, "modeldeprecated"),
		strings.Contains(lower, "has been deprecated"),
		strings.Contains(lower, "model has been retired"):
		reason = "model deprecated"
		modelGone = true
	case resp.StatusCode == http.StatusNotFound &&
		(strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "unavailable") || strings.Contains(lower, "does not exist"))):
		reason = "model not found"
		modelGone = true
	case resp.StatusCode == http.StatusForbidden && strings.Contains(text, "FreeTierError"):
		resp.Exit.Block(blockFor, "FreeTierError")
		reason = "free tier rejected the request shape or exit"
	case resp.StatusCode == http.StatusTooManyRequests || strings.Contains(text, "FreeUsageLimitError"):
		resp.Exit.Block(quotaFor, "free usage limit")
		reason = "free usage limit reached"
	case resp.StatusCode == http.StatusUnauthorized:
		reason = "upstream rejected the credential"
	case resp.StatusCode >= 500:
		resp.Exit.Block(15*time.Second, "upstream error")
		reason = "upstream server error"
	default:
		reason = fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
	}
	return reason, body, modelGone
}

// Endpoint renders the upstream URL for a model, protocol and lane.
func Endpoint(cfg config.Config, model catalog.Model, protocol catalog.Protocol, modelID string) string {
	var base string
	if cfg.Lane == "v1" {
		base = strings.TrimRight(cfg.Upstream, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
	} else {
		base = strings.TrimRight(model.BaseURL, "/")
		if cfg.UpstreamMode == "fixed" || base == "" {
			base = strings.TrimRight(cfg.Upstream, "/")
		}
		if !strings.Contains(base, "/v1") && !strings.Contains(base, "/v1beta") {
			switch protocol {
			case catalog.Anthropic:
				base += "/anthropic/v1"
			case catalog.Gemini:
				base += "/google/v1beta"
			default:
				base += "/openai/v1"
			}
		}
	}
	return catalog.Endpoint(catalog.Model{BaseURL: base}, protocol, modelID)
}
