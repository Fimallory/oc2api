package zen

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed tools.json
var toolsJSON []byte

// CoreToolNames are the tools the v2 free-tier gate requires on every request.
var CoreToolNames = []string{"shell", "edit", "glob", "grep", "read"}

type toolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var coreTools = func() map[string]toolDefinition {
	var defs map[string]toolDefinition
	if err := json.Unmarshal(toolsJSON, &defs); err != nil {
		panic("zen: invalid embedded tools.json: " + err.Error())
	}
	return defs
}()

// ChatTools returns the core tools in OpenAI Chat Completions shape.
func ChatTools() []any {
	out := make([]any, 0, len(CoreToolNames))
	for _, name := range CoreToolNames {
		def := coreTools[name]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": def.Description,
				"parameters":  def.Parameters,
				"strict":      false,
			},
		})
	}
	return out
}

// ResponsesTools returns the core tools in OpenAI Responses shape.
func ResponsesTools() []any {
	out := make([]any, 0, len(CoreToolNames))
	for _, name := range CoreToolNames {
		def := coreTools[name]
		out = append(out, map[string]any{
			"type":        "function",
			"name":        name,
			"description": def.Description,
			"parameters":  def.Parameters,
			"strict":      false,
		})
	}
	return out
}

// AnthropicTools returns the core tools in Anthropic Messages shape.
func AnthropicTools() []any {
	out := make([]any, 0, len(CoreToolNames))
	for _, name := range CoreToolNames {
		def := coreTools[name]
		out = append(out, map[string]any{
			"name":         name,
			"description":  def.Description,
			"input_schema": def.Parameters,
		})
	}
	return out
}

// ensureTools appends the core tools that the payload does not declare and
// returns the names it added. A missing "tools" key is created; a non-array
// value is left untouched, matching the reference implementation.
func ensureTools(payload map[string]any, build func() []any) []string {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = build()
		return append([]string(nil), CoreToolNames...)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	present := make(map[string]bool, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := entry["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				present[name] = true
			}
			continue
		}
		if name, ok := entry["name"].(string); ok {
			present[name] = true
		}
	}
	var added []string
	for _, name := range CoreToolNames {
		if present[name] {
			continue
		}
		added = append(added, name)
	}
	if len(added) == 0 {
		return nil
	}
	extra := build()
	for _, item := range extra {
		entry, _ := item.(map[string]any)
		name := ""
		if fn, ok := entry["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		} else {
			name, _ = entry["name"].(string)
		}
		for _, missing := range added {
			if name == missing {
				items = append(items, item)
				break
			}
		}
	}
	payload["tools"] = items
	return added
}

// ShapeChat normalizes a Chat Completions body for the free tier and returns
// the names of the tools it injected.
func ShapeChat(payload map[string]any, reasoningEffort string) []string {
	payload["stream"] = true
	if _, ok := payload["stream_options"].(map[string]any); !ok {
		payload["stream_options"] = map[string]any{"include_usage": true}
	} else {
		payload["stream_options"].(map[string]any)["include_usage"] = true
	}
	if _, ok := payload["max_completion_tokens"]; !ok {
		if _, ok := payload["max_tokens"]; !ok {
			payload["max_completion_tokens"] = 32000
		}
	}
	if reasoningEffort != "" {
		if _, ok := payload["reasoning_effort"]; !ok {
			payload["reasoning_effort"] = reasoningEffort
		}
	}
	return ensureTools(payload, ChatTools)
}

// ShapeResponses normalizes a Responses body for the free tier.
func ShapeResponses(payload map[string]any, reasoningEffort string) []string {
	payload["stream"] = true
	if _, ok := payload["max_output_tokens"]; !ok {
		payload["max_output_tokens"] = 32000
	}
	if reasoningEffort != "" {
		if _, ok := payload["reasoning"].(map[string]any); !ok {
			payload["reasoning"] = map[string]any{"effort": reasoningEffort}
		}
	}
	return ensureTools(payload, ResponsesTools)
}

// ShapeAnthropic normalizes a Messages body for the free tier.
func ShapeAnthropic(payload map[string]any, reasoningEffort string) []string {
	payload["stream"] = true
	if _, ok := payload["max_tokens"]; !ok {
		payload["max_tokens"] = 32000
	}
	if reasoningEffort != "" {
		if _, ok := payload["thinking"]; !ok {
			payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 8192}
		}
	}
	return ensureTools(payload, AnthropicTools)
}

// Wire is the header set of one upstream attempt.
type Wire struct {
	Headers map[string]string
	Query   string
}

// V2Wire renders the OpenCode 2.x CLI header set.
func V2Wire(session, project string, anthropic bool) Wire {
	headers := map[string]string{
		"User-Agent":            "opencode/latest/2.1.1/cli",
		"x-opencode-client":     "cli",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-session-affinity":    session,
		"X-Session-Id":          session,
		"x-opencode-project":    project,
	}
	wire := Wire{Headers: headers}
	if anthropic {
		headers["anthropic-version"] = "2023-06-01"
		headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		wire.Query = "beta=true"
	}
	return wire
}

// V1Wire renders the OpenCode 1.x AI SDK header set used by the /zen lane.
func V1Wire(session, project, messageID string, anthropic bool) Wire {
	sdk := "4.0.23"
	if anthropic {
		sdk = "4.0.46"
	}
	headers := map[string]string{
		"User-Agent":            "opencode/1.19.3 ai-sdk/provider-utils/" + sdk + " runtime/bun/1.3.14",
		"x-opencode-client":     "cli",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-opencode-request":    messageID,
		"x-opencode-project":    project,
	}
	if anthropic {
		headers["anthropic-version"] = "2023-06-01"
	}
	return Wire{Headers: headers}
}

// FilterInjected drops streamed tool calls that target tools this gateway
// injected but the client never declared, so a client is never asked to run a
// tool it does not have.
func FilterInjected(name string, injected []string, clientTools map[string]bool) bool {
	if clientTools[name] {
		return false
	}
	for _, n := range injected {
		if n == name {
			return true
		}
	}
	return false
}

// HasTool reports whether the payload declares a tool by name.
func HasTool(payload map[string]any, name string) bool {
	items, ok := payload["tools"].([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := entry["function"].(map[string]any); ok {
			if got, _ := fn["name"].(string); got == name {
				return true
			}
		}
		if got, _ := entry["name"].(string); got == name {
			return true
		}
	}
	return false
}

// NormalizeProxy turns a proxy entry into a URL the HTTP transport accepts.
func NormalizeProxy(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ""
	}
	if strings.Contains(entry, "://") {
		return entry
	}
	return "http://" + entry
}
