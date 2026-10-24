package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the on-disk / environment configuration of the gateway.
type Config struct {
	// Listen is the local address the gateway binds to.
	Listen string `json:"listen"`
	// LocalKey, when set, is required as the bearer token from local clients.
	LocalKey string `json:"local_key"`
	// Upstream is the Zen inference origin. The v2 lane is /inference.
	Upstream string `json:"upstream"`
	// UpstreamMode selects where endpoints come from: "catalog" uses the
	// per-model base URL from models.opencode.ai, "fixed" always uses Upstream.
	UpstreamMode string `json:"upstream_mode"`
	// Lane selects the upstream request shape: "v2" (/inference) or "v1" (/zen).
	Lane string `json:"lane"`
	// APIKey is the upstream credential. Empty means the anonymous "public" tier.
	APIKey string `json:"api_key"`
	// Proxies is the ordered exit pool tried after the direct connection.
	// Entries accept "host:port", "user:pass@host:port" and full URLs.
	Proxies []string `json:"proxies"`
	// Direct disables the direct connection when false.
	Direct bool `json:"direct"`
	// FreeOnly restricts the gateway to zero-cost models.
	FreeOnly bool `json:"free_only"`
	// RequestTimeoutSeconds bounds one upstream attempt; 0 means no bound.
	RequestTimeoutSeconds int `json:"request_timeout_seconds"`
	// MaxAttempts caps upstream attempts per client request.
	MaxAttempts int `json:"max_attempts"`
	// BlockSeconds is how long an exit that returned FreeTierError is skipped.
	BlockSeconds int `json:"block_seconds"`
	// QuotaSeconds is how long an exit that returned a free-usage-limit is skipped.
	QuotaSeconds int `json:"quota_seconds"`
	// CatalogRefreshMinutes is the model catalog refresh interval.
	CatalogRefreshMinutes int `json:"catalog_refresh_minutes"`
	// ProbeOnStart tests every free model with a minimal request at startup.
	ProbeOnStart bool `json:"probe_on_start"`
	// ProbeConcurrency is the number of models probed in parallel.
	ProbeConcurrency int `json:"probe_concurrency"`
	// ProbeTimeoutSeconds bounds one probe request.
	ProbeTimeoutSeconds int `json:"probe_timeout_seconds"`
	// ProbeIntervalMS spaces probes out so they do not trip rate limiting.
	ProbeIntervalMS int `json:"probe_interval_ms"`
	// CacheEnabled turns the request-hash response cache on.
	CacheEnabled bool `json:"cache_enabled"`
	// CacheTTLSeconds is how long a cached response stays valid.
	CacheTTLSeconds int `json:"cache_ttl_seconds"`
	// CacheMaxEntries bounds the number of cached responses.
	CacheMaxEntries int `json:"cache_max_entries"`
	// Debug logs every upstream attempt.
	Debug bool `json:"debug"`
	// ReasoningEffort is the default reasoning_effort sent upstream ("" to omit).
	ReasoningEffort string `json:"reasoning_effort"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Listen:                "127.0.0.1:8787",
		Upstream:              "https://opencode.ai/inference",
		UpstreamMode:          "catalog",
		Lane:                  "v2",
		Direct:                true,
		FreeOnly:              true,
		RequestTimeoutSeconds: 0,
		MaxAttempts:           0,
		BlockSeconds:          900,
		QuotaSeconds:          60,
		CatalogRefreshMinutes: 10,
		ReasoningEffort:       "",
		ProbeOnStart:          true,
		ProbeConcurrency:      1,
		ProbeTimeoutSeconds:   30,
		ProbeIntervalMS:       700,
		CacheEnabled:          true,
		CacheTTLSeconds:       1800,
		CacheMaxEntries:       500,
	}
}

// Load reads path when it exists, then applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// A missing file is not an error: environment variables may be enough.
		default:
			return cfg, err
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("OC2API_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("OC2API_LOCAL_KEY"); v != "" {
		cfg.LocalKey = v
	}
	if v := os.Getenv("OC2API_UPSTREAM"); v != "" {
		cfg.Upstream = v
	}
	if v := os.Getenv("OC2API_UPSTREAM_MODE"); v != "" {
		cfg.UpstreamMode = v
	}
	if v := os.Getenv("OC2API_LANE"); v != "" {
		cfg.Lane = v
	}
	if v := os.Getenv("OC2API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("OC2API_PROXIES"); v != "" {
		cfg.Proxies = splitList(v)
	}
	if v := os.Getenv("OC2API_DIRECT"); v != "" {
		cfg.Direct = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("OC2API_FREE_ONLY"); v != "" {
		cfg.FreeOnly = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("OC2API_DEBUG"); v != "" {
		cfg.Debug = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("OC2API_REASONING_EFFORT"); v != "" {
		cfg.ReasoningEffort = v
	}
	if v := os.Getenv("OC2API_PROBE"); v != "" {
		cfg.ProbeOnStart = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("OC2API_PROBE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ProbeConcurrency = n
		}
	}
	if v := os.Getenv("OC2API_CACHE"); v != "" {
		cfg.CacheEnabled = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("OC2API_CACHE_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CacheTTLSeconds = n
		}
	}
	if v := os.Getenv("OC2API_CACHE_MAX_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CacheMaxEntries = n
		}
	}
	if v := os.Getenv("OC2API_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxAttempts = n
		}
	}
	if v := os.Getenv("OC2API_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RequestTimeoutSeconds = n
		}
	}
}

func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Validate normalizes and checks the configuration.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	c.UpstreamMode = strings.ToLower(strings.TrimSpace(c.UpstreamMode))
	if c.UpstreamMode == "" {
		c.UpstreamMode = "catalog"
	}
	if c.UpstreamMode != "catalog" && c.UpstreamMode != "fixed" {
		return fmt.Errorf("upstream_mode must be catalog or fixed, got %q", c.UpstreamMode)
	}
	c.Lane = strings.ToLower(strings.TrimSpace(c.Lane))
	if c.Lane != "v1" && c.Lane != "v2" {
		return fmt.Errorf("lane must be v1 or v2, got %q", c.Lane)
	}
	c.Upstream = strings.TrimRight(strings.TrimSpace(c.Upstream), "/")
	if c.Upstream == "" {
		return errors.New("upstream is required")
	}
	if c.Lane == "v1" && c.Upstream == "https://opencode.ai/inference" {
		c.Upstream = "https://opencode.ai/zen"
	}
	if c.RequestTimeoutSeconds < 0 || c.BlockSeconds < 0 || c.QuotaSeconds < 0 || c.MaxAttempts < 0 {
		return errors.New("durations and attempt limits cannot be negative")
	}
	if c.CatalogRefreshMinutes <= 0 {
		c.CatalogRefreshMinutes = 10
	}
	if c.ProbeConcurrency <= 0 {
		c.ProbeConcurrency = 1
	}
	if c.ProbeIntervalMS < 0 {
		c.ProbeIntervalMS = 0
	}
	if c.ProbeTimeoutSeconds <= 0 {
		c.ProbeTimeoutSeconds = 30
	}
	if c.CacheTTLSeconds <= 0 {
		c.CacheTTLSeconds = 1800
	}
	if c.CacheMaxEntries < 0 {
		c.CacheMaxEntries = 0
	}
	if !c.Direct && len(c.Proxies) == 0 {
		return errors.New("direct is disabled and no proxies are configured")
	}
	return nil
}

// Timeout is the per-attempt timeout, zero when unbounded.
func (c Config) Timeout() time.Duration {
	if c.RequestTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}
