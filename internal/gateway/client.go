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
type Exit struct {
	Name string
	URL  *url.URL
	HTTP *http.Client

	mu           sync.Mutex
	blockedUntil time.Time
	reason       string
}

// Available reports whether the exit may be used now.
func (e *Exit) Available(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.After(e.blockedUntil)
}

// Block marks the exit unusable for d.
func (e *Exit) Block(d time.Duration, reason string) {
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
	state := map[string]any{"name": e.Name, "blocked": time.Now().Before(e.blockedUntil)}
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

// NewPool builds the direct exit and one exit per proxy entry.
func NewPool(cfg config.Config) (*Pool, error) {
	pool := &Pool{}
	if cfg.Direct {
		pool.exits = append(pool.exits, newExit("direct", nil))
	}
	for index, raw := range cfg.Proxies {
		normalized := zen.NormalizeProxy(raw)
		parsed, err := url.Parse(normalized)
		if err != nil {
			return nil, fmt.Errorf("proxy %d (%s): %w", index+1, redact(raw), err)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("proxy %d (%s): missing host", index+1, redact(raw))
		}
		pool.exits = append(pool.exits, newExit(fmt.Sprintf("proxy-%d", index+1), parsed))
	}
	if len(pool.exits) == 0 {
		return nil, fmt.Errorf("no exits configured")
	}
	return pool, nil
}

func newExit(name string, proxy *url.URL) *Exit {
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
	return &Exit{
		Name: name,
		URL:  proxy,
		HTTP: &http.Client{Transport: transport},
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
	resp, err := attempt.Exit.HTTP.Do(req)
	if err != nil {
		attempt.Exit.Block(15*time.Second, "transport error")
		return nil, err
	}
	return &Response{Response: resp, Exit: attempt.Exit}, nil
}

// Classify inspects a non-2xx response and blocks the exit accordingly. It
// returns a short human-readable reason and the body (rewound) for the caller.
func Classify(resp *Response, blockFor, quotaFor time.Duration) (reason string, body []byte) {
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	text := string(body)
	switch {
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
	return reason, body
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
