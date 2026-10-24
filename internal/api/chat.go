package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// BuildChat renders a Request as an OpenAI Chat Completions body.
func BuildChat(req *Request) map[string]any {
	payload := map[string]any{
		"model":    req.Model,
		"messages": chatMessages(req),
		"stream":   true,
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			fn := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				fn["description"] = tool.Description
			}
			if tool.Schema != nil {
				fn["parameters"] = tool.Schema
			}
			if tool.Strict {
				fn["strict"] = true
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		payload["tools"] = tools
		if req.ToolChoice != nil {
			payload["tool_choice"] = req.ToolChoice
		}
	}
	if req.MaxOutputTokens > 0 {
		payload["max_completion_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		payload["reasoning_effort"] = req.ReasoningEffort
	}
	return payload
}

func chatMessages(req *Request) []any {
	var out []any
	system := strings.TrimSpace(req.Instructions)
	for _, message := range req.Messages {
		if message.Role == "system" {
			if text := message.Text(); text != "" {
				if system != "" {
					system += "\n\n"
				}
				system += text
			}
		}
	}
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, message := range req.Messages {
		switch message.Role {
		case "system":
			continue
		case "tool":
			for _, part := range message.Parts {
				if part.Type == "tool_result" {
					out = append(out, map[string]any{
						"role":         "tool",
						"tool_call_id": part.CallID,
						"content":      part.Text,
					})
				}
			}
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			if text := message.Text(); text != "" {
				entry["content"] = text
			}
			var calls []any
			for _, part := range message.Parts {
				if part.Type != "tool_call" {
					continue
				}
				calls = append(calls, map[string]any{
					"id":   part.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      part.Name,
						"arguments": part.Arguments,
					},
				})
			}
			if len(calls) > 0 {
				entry["tool_calls"] = calls
			}
			if entry["content"] == nil && len(calls) == 0 {
				continue
			}
			out = append(out, entry)
		default:
			out = append(out, map[string]any{
				"role":    message.Role,
				"content": chatContent(message),
			})
		}
	}
	return out
}

func chatContent(message Message) any {
	if len(message.Parts) == 1 && message.Parts[0].Type == "text" {
		return message.Parts[0].Text
	}
	parts := make([]any, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
		case "image":
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": part.ImageURL},
			})
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return parts
}

// chatChunk is one OpenAI Chat Completions stream chunk.
type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// chatStreamState carries per-stream bookkeeping across SSE chunks.
type chatStreamState struct {
	started      map[int]bool
	finishReason string
}

// ParseChatStream converts an OpenAI Chat Completions SSE stream into events.
func ParseChatStream(r io.Reader, emit func(Event)) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	state := &chatStreamState{started: map[int]bool{}}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					for index := range state.started {
						emit(Event{Kind: EventToolEnd, ToolIndex: index})
					}
					emit(Event{Kind: EventDone, FinishReason: state.finishReason})
					return nil
				}
				if data != "" {
					handleChatData(data, emit, state)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				for index := range state.started {
					emit(Event{Kind: EventToolEnd, ToolIndex: index})
				}
				emit(Event{Kind: EventDone, FinishReason: state.finishReason})
				return nil
			}
			return err
		}
	}
}

func handleChatData(data string, emit func(Event), state *chatStreamState) {
	var chunk chatChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Error != nil {
		emit(Event{Kind: EventError, Err: fmt.Errorf("%s: %s", chunk.Error.Type, chunk.Error.Message)})
		return
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta.ReasoningContent != "" {
			emit(Event{Kind: EventReasoning, Text: delta.ReasoningContent})
		} else if delta.Reasoning != "" {
			emit(Event{Kind: EventReasoning, Text: delta.Reasoning})
		}
		if delta.Content != "" {
			emit(Event{Kind: EventText, Text: delta.Content})
		}
		for _, call := range delta.ToolCalls {
			if !state.started[call.Index] {
				state.started[call.Index] = true
				emit(Event{Kind: EventToolStart, ToolIndex: call.Index, ToolID: call.ID, ToolName: call.Function.Name})
			} else if call.Function.Name != "" {
				emit(Event{Kind: EventToolStart, ToolIndex: call.Index, ToolName: call.Function.Name})
			}
			if call.Function.Arguments != "" {
				emit(Event{Kind: EventToolArgs, ToolIndex: call.Index, Text: call.Function.Arguments})
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			state.finishReason = *choice.FinishReason
		}
	}
	if chunk.Usage != nil {
		emit(Event{Kind: EventUsage, Usage: &Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
			Reasoning:    chunk.Usage.CompletionTokensDetails.ReasoningTokens,
			CachedTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
		}})
	}
}
