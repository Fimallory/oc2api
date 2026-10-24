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
	mu        sync.RWMutex
	byPublic  map[string]Model
	byID      map[string]Model
	disabled  map[string]string
	source    string
	fetched   time.Time
	liveCount int
	liveErr   error
	err       error
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
		disabled: map[string]string{},
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
			live := c.liveCount
			c.mu.RUnlock()
			logf("catalog loaded: %d free models (%d live upstream)", n, live)
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
	live, liveErr := c.fetchLive(ctx)
	byPublic, byID, err := parse(raw, live)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.byPublic = byPublic
	c.byID = byID
	c.liveCount = len(live)
	c.liveErr = liveErr
	c.fetched = time.Now()
	c.err = nil
	c.mu.Unlock()
	return nil
}

// liveSource lists the models the inference gateway currently serves. It needs
// no credential and is the cheapest way to drop deprecated models: the catalog
// keeps advertising retired ids long after the gateway stops serving them.
const liveSource = "https://opencode.ai/inference/v1/models"

type liveFile struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (c *Catalog) fetchLive(ctx context.Context) (map[string]bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, liveSource, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "opencode/latest/2.1.1/cli")
	req.Header.Set("x-opencode-client", "cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live models: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var file liveFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(file.Data))
	for _, item := range file.Data {
		if item.ID != "" {
			live[item.ID] = true
		}
	}
	return live, nil
}

// Disable removes a model from the published catalog. A startup probe or an
// upstream ModelDeprecated response is authoritative until the process
// restarts, because a catalog refresh would otherwise resurrect it.
func (c *Catalog) Disable(publicID, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled == nil {
		c.disabled = map[string]string{}
	}
	c.disabled[publicID] = reason
}

// IsDisabled reports whether a published model was removed.
func (c *Catalog) IsDisabled(publicID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.disabled[publicID]
	return ok
}

// Disabled returns the removed models and their reasons.
func (c *Catalog) Disabled() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.disabled))
	for id, reason := range c.disabled {
		out[id] = reason
	}
	return out
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

func parse(raw []byte, live map[string]bool) (map[string]Model, map[string]Model, error) {
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
		// The live list is authoritative: a catalog entry the gateway no
		// longer serves is a deprecated model, not a usable one.
		if len(live) > 0 && !live[id] {
			continue
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
	_, removed := c.disabled[id]
	byPublic := c.byPublic[id]
	byID, hasID := c.byID[id]
	_, publicHit := c.byPublic[id]
	c.mu.RUnlock()
	if removed {
		return Model{}, false
	}

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
	for id, model := range c.byPublic {
		if _, removed := c.disabled[id]; removed {
			continue
		}
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

// LiveCount reports how many ids the upstream gateway listed.
func (c *Catalog) LiveCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.liveCount
}

// LiveErr reports why the live list could not be fetched.
func (c *Catalog) LiveErr() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.liveErr
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
