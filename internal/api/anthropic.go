package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// BuildAnthropic renders a Request as an Anthropic Messages body.
func BuildAnthropic(req *Request) map[string]any {
	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": maxInt(req.MaxOutputTokens, 32000),
		"stream":     true,
		"messages":   anthropicMessages(req),
	}
	if system := strings.TrimSpace(req.Instructions); system != "" {
		payload["system"] = system
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			entry := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				entry["description"] = tool.Description
			}
			if tool.Schema != nil {
				entry["input_schema"] = tool.Schema
			} else {
				entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, entry)
		}
		payload["tools"] = tools
		if choice := anthropicToolChoice(req.ToolChoice); choice != nil {
			payload["tool_choice"] = choice
		}
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		budget := 8192
		if req.MaxOutputTokens > 0 && req.MaxOutputTokens < budget*2 {
			budget = req.MaxOutputTokens / 2
		}
		if budget >= 1024 {
			payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		}
	}
	return payload
}

func anthropicToolChoice(choice any) map[string]any {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return nil
		}
	case map[string]any:
		if name := StringAt(v, "name"); name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
	}
	return nil
}

func anthropicMessages(req *Request) []any {
	var out []any
	var pendingResults []any
	flushResults := func() {
		if len(pendingResults) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": pendingResults})
		pendingResults = nil
	}
	for _, message := range req.Messages {
		switch message.Role {
		case "system":
			continue
		case "tool":
			for _, part := range message.Parts {
				if part.Type != "tool_result" {
					continue
				}
				pendingResults = append(pendingResults, map[string]any{
					"type":        "tool_result",
					"tool_use_id": part.CallID,
					"content":     part.Text,
				})
			}
		case "assistant":
			flushResults()
			var blocks []any
			if text := message.Text(); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, part := range message.Parts {
				if part.Type != "tool_call" {
					continue
				}
				var input any = map[string]any{}
				if part.Arguments != "" {
					if err := json.Unmarshal([]byte(part.Arguments), &input); err != nil {
						input = map[string]any{"input": part.Arguments}
					}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    part.CallID,
					"name":  part.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			flushResults()
			out = append(out, map[string]any{"role": "user", "content": anthropicContent(message)})
		}
	}
	flushResults()
	return out
}

func anthropicContent(message Message) any {
	if len(message.Parts) == 1 && message.Parts[0].Type == "text" {
		return message.Parts[0].Text
	}
	blocks := make([]any, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		case "image":
			if strings.HasPrefix(part.ImageURL, "data:") {
				mediaType, data := splitDataURL(part.ImageURL)
				blocks = append(blocks, map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
				})
			} else {
				blocks = append(blocks, map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "url", "url": part.ImageURL},
				})
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return blocks
}

func splitDataURL(url string) (string, string) {
	rest := strings.TrimPrefix(url, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return "image/png", rest
	}
	mediaType := strings.TrimSuffix(parts[0], ";base64")
	return mediaType, parts[1]
}

// anthropicEvent is one Messages SSE frame.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
		InputTokens  int `json:"input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseAnthropicStream converts a Messages SSE stream into events.
func ParseAnthropicStream(r io.Reader, emit func(Event)) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	state := struct {
		tools     map[int]bool
		input     int
		output    int
		stop      string
		blockType map[int]string
	}{tools: map[int]bool{}, blockType: map[int]string{}}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "" {
					handleAnthropicData(data, emit, &state)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				for index := range state.tools {
					emit(Event{Kind: EventToolEnd, ToolIndex: index})
				}
				emit(Event{Kind: EventDone, FinishReason: state.stop})
				return nil
			}
			return err
		}
	}
}

func handleAnthropicData(data string, emit func(Event), state *struct {
	tools     map[int]bool
	input     int
	output    int
	stop      string
	blockType map[int]string
}) {
	var event anthropicEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			state.input = event.Message.Usage.InputTokens
		}
	case "content_block_start":
		state.blockType[event.Index] = event.ContentBlock.Type
		if event.ContentBlock.Type == "tool_use" {
			state.tools[event.Index] = true
			emit(Event{Kind: EventToolStart, ToolIndex: event.Index, ToolID: event.ContentBlock.ID, ToolName: event.ContentBlock.Name})
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			emit(Event{Kind: EventText, Text: event.Delta.Text})
		case "thinking_delta":
			emit(Event{Kind: EventReasoning, Text: event.Delta.Thinking})
		case "input_json_delta":
			emit(Event{Kind: EventToolArgs, ToolIndex: event.Index, Text: event.Delta.PartialJSON})
		}
	case "content_block_stop":
		if state.tools[event.Index] {
			emit(Event{Kind: EventToolEnd, ToolIndex: event.Index})
		}
	case "message_delta":
		if event.Delta.StopReason != "" {
			state.stop = event.Delta.StopReason
		}
		if event.Usage != nil {
			state.output = event.Usage.OutputTokens
			if event.Usage.InputTokens > 0 {
				state.input = event.Usage.InputTokens
			}
		}
	case "message_stop":
		emit(Event{Kind: EventUsage, Usage: &Usage{
			InputTokens:  state.input,
			OutputTokens: state.output,
			TotalTokens:  state.input + state.output,
		}})
		for index := range state.tools {
			emit(Event{Kind: EventToolEnd, ToolIndex: index})
		}
		emit(Event{Kind: EventDone, FinishReason: state.stop})
	case "error":
		if event.Error != nil {
			emit(Event{Kind: EventError, Err: fmt.Errorf("%s: %s", event.Error.Type, event.Error.Message)})
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
