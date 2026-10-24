package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// hashValue returns the SHA-256 of the canonical JSON encoding of value.
func hashValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte("unhashable")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// randomHex returns n random bytes encoded as hex.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "0"
	}
	return hex.EncodeToString(buf)
}

// CacheKey builds the canonical, content-addressed key for a request. Session
// and project identity are deliberately excluded: they change per attempt but
// never change the model output.
func CacheKey(req *Request) string {
	type toolKey struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Schema      map[string]any `json:"schema"`
		Strict      bool           `json:"strict"`
	}
	type key struct {
		Model        string    `json:"model"`
		Instructions string    `json:"instructions"`
		Messages     []Message `json:"messages"`
		Tools        []toolKey `json:"tools"`
		ToolChoice   any       `json:"tool_choice"`
		MaxTokens    int       `json:"max_tokens"`
		Temperature  *float64  `json:"temperature"`
		TopP         *float64  `json:"top_p"`
		Reasoning    string    `json:"reasoning"`
		Stream       bool      `json:"stream"`
	}
	tools := make([]toolKey, 0, len(req.Tools))
	for _, tool := range req.Tools {
		tools = append(tools, toolKey{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      tool.Schema,
			Strict:      tool.Strict,
		})
	}
	return hashValue(key{
		Model:        req.Model,
		Instructions: req.Instructions,
		Messages:     req.Messages,
		Tools:        tools,
		ToolChoice:   req.ToolChoice,
		MaxTokens:    req.MaxOutputTokens,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Reasoning:    req.ReasoningEffort,
		Stream:       req.Stream,
	})
}
