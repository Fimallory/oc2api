// Package catalog resolves model ids to upstream protocols using OpenCode's
// public model catalog.
//
// The gateway only serves the free/anonymous tier, so the catalog is filtered
// to zero-cost models and their ids are published without the "-free" suffix
// ("glm-5.3-flash-free" is exposed as "glm-5.3-flash"). The upstream request
// still uses the real id.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Protocol is the wire protocol an upstream model speaks.
type Protocol string

const (
	Chat      Protocol = "chat"
	Responses Protocol = "responses"
	Anthropic Protocol = "anthropic"
	Gemini    Protocol = "gemini"
)

// Model is one catalog entry.
type Model struct {
	// ID is the upstream model id, e.g. "glm-5.3-flash-free".
	ID string `json:"id"`
	// PublicID is the id clients use, e.g. "glm-5.3-flash".
	PublicID string   `json:"public_id"`
	Protocol Protocol `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	Free     bool     `json:"free"`
	Name     string   `json:"name,omitempty"`
}

// Catalog is a cached snapshot of the OpenCode model catalog.
type Catalog struct {
	mu       sync.RWMutex
	byPublic map[string]Model
	byID     map[string]Model
	source   string
	fetched  time.Time
	err      error
}

const defaultSource = "https://models.opencode.ai/api.json"

// New creates an empty catalog.
func New(source string) *Catalog {
	if source == "" {
		source = defaultSource
	}
	return &Catalog{
		source:   source,
		byPublic: map[string]Model{},
		byID:     map[string]Model{},
	}
}

// Start refreshes the catalog now and then on every interval until ctx ends.
func (c *Catalog) Start(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	refresh := func() {
		if err := c.Refresh(ctx); err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			if logf != nil {
				logf("catalog refresh failed: %v", err)
			}
			return
		}
		if logf != nil {
			c.mu.RLock()
			n := len(c.byPublic)
			c.mu.RUnlock()
			logf("catalog loaded: %d free models", n)
		}
	}
	refresh()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

// Refresh fetches and replaces the catalog snapshot.
func (c *Catalog) Refresh(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.source, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "oc2api/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catalog: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	byPublic, byID, err := parse(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.byPublic = byPublic
	c.byID = byID
	c.fetched = time.Now()
	c.err = nil
	c.mu.Unlock()
	return nil
}

type catalogFile struct {
	OpenCode struct {
		NPM   string                     `json:"npm"`
		API   string                     `json:"api"`
		Name  string                     `json:"name"`
		Model map[string]catalogModelDef `json:"models"`
	} `json:"opencode"`
}

type catalogModelDef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider *struct {
		NPM string `json:"npm"`
		API string `json:"api"`
	} `json:"provider"`
	Cost struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
}

func parse(raw []byte) (map[string]Model, map[string]Model, error) {
	var file catalogFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, nil, err
	}
	byPublic := make(map[string]Model, len(file.OpenCode.Model))
	byID := make(map[string]Model, len(file.OpenCode.Model))
	for id, def := range file.OpenCode.Model {
		if def.ID != "" {
			id = def.ID
		}
		npm, api := file.OpenCode.NPM, file.OpenCode.API
		if def.Provider != nil {
			if def.Provider.NPM != "" {
				npm = def.Provider.NPM
			}
			if def.Provider.API != "" {
				api = def.Provider.API
			}
		}
		protocol := protocolFor(npm)
		free := def.Cost.Input == 0 && def.Cost.Output == 0
		model := Model{
			ID:       id,
			PublicID: publicID(id, free),
			Protocol: protocol,
			BaseURL:  baseURLFor(protocol, api),
			Free:     free,
			Name:     def.Name,
		}
		byID[id] = model
		// Only free/anonymous models are published; a paid model that shares
		// the stripped name must not shadow its free counterpart.
		if free {
			byPublic[model.PublicID] = model
		}
	}
	return byPublic, byID, nil
}

// publicID strips the "-free" suffix from free models so the gateway exposes
// the same model names the official client shows for the anonymous tier.
func publicID(id string, free bool) string {
	if !free {
		return id
	}
	return strings.TrimSuffix(id, "-free")
}

func protocolFor(npm string) Protocol {
	switch {
	case strings.Contains(npm, "anthropic"):
		return Anthropic
	case strings.Contains(npm, "google"):
		return Gemini
	case npm == "@ai-sdk/openai":
		return Responses
	default:
		return Chat
	}
}

func baseURLFor(protocol Protocol, catalogAPI string) string {
	if catalogAPI != "" {
		return strings.TrimRight(catalogAPI, "/")
	}
	switch protocol {
	case Anthropic:
		return "https://opencode.ai/inference/anthropic/v1"
	case Gemini:
		return "https://opencode.ai/inference/google/v1beta"
	default:
		return "https://opencode.ai/inference/openai/v1"
	}
}

// Lookup resolves a client model id. With freeOnly the published (suffix-free)
// name wins; otherwise an exact upstream id wins. Unknown ids fall back to a
// name heuristic so a catalog outage does not take the gateway down.
func (c *Catalog) Lookup(id string, freeOnly bool) (Model, bool) {
	c.mu.RLock()
	byPublic := c.byPublic[id]
	byID, hasID := c.byID[id]
	publicHit := false
	if _, ok := c.byPublic[id]; ok {
		publicHit = true
	}
	c.mu.RUnlock()

	if freeOnly {
		if publicHit {
			return byPublic, true
		}
		if hasID && byID.Free {
			return byID, true
		}
		return heuristic(id), false
	}
	if hasID {
		return byID, true
	}
	if publicHit {
		return byPublic, true
	}
	return heuristic(id), false
}

// Free reports whether the model is zero-cost according to the catalog.
func (c *Catalog) Free(id string) bool {
	model, ok := c.Lookup(id, true)
	return ok && model.Free
}

func heuristic(id string) Model {
	lower := strings.ToLower(id)
	protocol := Chat
	switch {
	case strings.HasPrefix(lower, "claude"), strings.Contains(lower, "minimax"), strings.HasPrefix(lower, "qwen"):
		protocol = Anthropic
	case strings.HasPrefix(lower, "gpt"), strings.HasPrefix(lower, "grok"), strings.Contains(lower, "codex"):
		protocol = Responses
	case strings.HasPrefix(lower, "gemini"):
		protocol = Gemini
	}
	free := isFreeName(lower)
	upstream := id
	if free && !strings.HasSuffix(lower, "-free") && lower != "big-pickle" && lower != "grok-code" {
		upstream = id + "-free"
	}
	return Model{
		ID:       upstream,
		PublicID: publicID(upstream, free),
		Protocol: protocol,
		BaseURL:  baseURLFor(protocol, ""),
		Free:     free,
	}
}

func isFreeName(id string) bool {
	return strings.Contains(id, "free") || id == "big-pickle" || id == "grok-code"
}

// List returns every published free model, sorted by public id.
func (c *Catalog) List() []Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Model, 0, len(c.byPublic))
	for _, model := range c.byPublic {
		out = append(out, model)
	}
	return out
}

// Endpoint renders the upstream URL for a model and protocol.
func Endpoint(model Model, protocol Protocol, modelID string) string {
	base := strings.TrimRight(model.BaseURL, "/")
	switch protocol {
	case Chat:
		return base + "/chat/completions"
	case Responses:
		return base + "/responses"
	case Anthropic:
		return base + "/messages"
	case Gemini:
		return base + "/models/" + modelID + ":streamGenerateContent?alt=sse"
	default:
		return base + "/chat/completions"
	}
}

// Err is the last refresh error, if any.
func (c *Catalog) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.err
}

// Fetched reports when the snapshot was last refreshed.
func (c *Catalog) Fetched() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetched
}
