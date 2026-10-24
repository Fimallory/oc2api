package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ParseResponses converts an OpenAI Responses API body into a Request.
func ParseResponses(raw []byte) (*Request, map[string]any, error) {
	payload, err := DecodeObject(raw)
	if err != nil {
		return nil, nil, err
	}
	model := StringAt(payload, "model")
	if model == "" {
		return nil, payload, fmt.Errorf("model is required")
	}
	req := &Request{
		Model:           model,
		Instructions:    StringAt(payload, "instructions"),
		ToolChoice:      payload["tool_choice"],
		MaxOutputTokens: IntAt(payload, "max_output_tokens"),
		Temperature:     FloatAt(payload, "temperature"),
		TopP:            FloatAt(payload, "top_p"),
		Stream:          BoolAt(payload, "stream"),
		Metadata:        MapAt(payload, "metadata"),
	}
	if reasoning := MapAt(payload, "reasoning"); reasoning != nil {
		req.ReasoningEffort = StringAt(reasoning, "effort")
	}

	switch input := payload["input"].(type) {
	case string:
		req.Messages = append(req.Messages, Message{Role: "user", Parts: []Part{{Type: "text", Text: input}}})
	case []any:
		req.Messages = parseResponsesItems(input)
	case nil:
		// No input: leave the conversation empty.
	default:
		return nil, payload, fmt.Errorf("input must be a string or an array")
	}

	for _, item := range SliceAt(payload, "tools") {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if StringAt(entry, "type") != "function" && StringAt(entry, "type") != "" {
			continue
		}
		name := StringAt(entry, "name")
		if name == "" {
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        name,
			Description: StringAt(entry, "description"),
			Schema:      MapAt(entry, "parameters"),
			Strict:      BoolAt(entry, "strict"),
		})
	}
	return req, payload, nil
}

func parseResponsesItems(items []any) []Message {
	var out []Message
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch StringAt(item, "type") {
		case "function_call":
			call := Part{
				Type:      "tool_call",
				CallID:    firstNonEmpty(StringAt(item, "call_id"), StringAt(item, "id")),
				Name:      StringAt(item, "name"),
				Arguments: StringAt(item, "arguments"),
			}
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].Parts = append(out[n-1].Parts, call)
			} else {
				out = append(out, Message{Role: "assistant", Parts: []Part{call}})
			}
		case "function_call_output":
			out = append(out, Message{Role: "tool", Parts: []Part{{
				Type:   "tool_result",
				CallID: firstNonEmpty(StringAt(item, "call_id"), StringAt(item, "id")),
				Text:   outputText(item["output"]),
			}}})
		case "reasoning":
			// Reasoning items are not replayed: the upstream regenerates them.
		case "message", "":
			role := StringAt(item, "role")
			if role == "" {
				continue
			}
			out = append(out, Message{Role: normalizeRole(role), Parts: parseContent(item["content"])})
		default:
			role := StringAt(item, "role")
			if role == "" {
				continue
			}
			out = append(out, Message{Role: normalizeRole(role), Parts: parseContent(item["content"])})
		}
	}
	return out
}

func normalizeRole(role string) string {
	switch role {
	case "developer":
		return "system"
	case "assistant", "user", "system", "tool":
		return role
	default:
		return "user"
	}
}

func parseContent(content any) []Part {
	switch v := content.(type) {
	case string:
		return []Part{{Type: "text", Text: v}}
	case []any:
		var parts []Part
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch StringAt(part, "type") {
			case "input_text", "output_text", "text", "":
				parts = append(parts, Part{Type: "text", Text: StringAt(part, "text")})
			case "input_image", "image_url":
				url := StringAt(part, "image_url")
				if url == "" {
					url = StringAt(MapAt(part, "image_url"), "url")
				}
				if url != "" {
					parts = append(parts, Part{Type: "image", ImageURL: url})
				}
			}
		}
		return parts
	}
	return nil
}

func outputText(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case []any:
		var b strings.Builder
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := StringAt(part, "text"); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		if v == nil {
			return ""
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// SessionSignalFromHeaders extracts a client-supplied conversation identity.
func SessionSignalFromHeaders(h http.Header) string {
	for _, name := range []string{
		"x-opencode-session", "x-opencode-session-id", "x-session-affinity",
		"x-session-id", "session_id", "conversation-id", "conversation_id",
	} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}
