package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Part is one piece of a message.
type Part struct {
	Type      string // text | image | tool_call | tool_result
	Text      string
	ImageURL  string
	CallID    string
	Name      string
	Arguments string
}

// Message is one conversation turn in protocol-neutral form.
type Message struct {
	Role  string // system | user | assistant | tool
	Parts []Part
}

// Text returns the concatenated text of a message.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Tool is one function tool offered to the model.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Strict      bool
}

// Request is a protocol-neutral inference request.
type Request struct {
	Model           string
	Instructions    string
	Messages        []Message
	Tools           []Tool
	ToolChoice      any
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
	Stream          bool
	ReasoningEffort string
	Metadata        map[string]any
	SessionSignal   string
	ProjectSignal   string
}

// Usage is token accounting as reported by the upstream.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	Reasoning    int `json:"reasoning_tokens,omitempty"`
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// EventKind enumerates protocol-neutral stream events.
type EventKind int

const (
	EventText EventKind = iota
	EventReasoning
	EventToolStart
	EventToolArgs
	EventToolEnd
	EventUsage
	EventDone
	EventError
)

// Event is one protocol-neutral stream event.
type Event struct {
	Kind         EventKind
	Text         string
	ToolIndex    int
	ToolID       string
	ToolName     string
	FinishReason string
	Usage        *Usage
	Err          error
}

// Helpers for defensive JSON access.

func MapAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func StringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func SliceAt(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func IntAt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func FloatAt(m map[string]any, key string) *float64 {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case float64:
		return &v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

func BoolAt(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

// FirstUserText returns the text of the first user turn.
func FirstUserText(messages []Message) string {
	for _, m := range messages {
		if m.Role == "user" {
			return m.Text()
		}
	}
	return ""
}

// DecodeObject decodes raw JSON into a map, rejecting non-objects.
func DecodeObject(raw []byte) (map[string]any, error) {
	var payload map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return payload, nil
}
