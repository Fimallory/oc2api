package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// BuildResponses renders a Request as an OpenAI Responses body.
func BuildResponses(req *Request) map[string]any {
	payload := map[string]any{
		"model":  req.Model,
		"input":  responsesInput(req),
		"stream": true,
	}
	if req.Instructions != "" {
		payload["instructions"] = req.Instructions
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			entry := map[string]any{"type": "function", "name": tool.Name}
			if tool.Description != "" {
				entry["description"] = tool.Description
			}
			if tool.Schema != nil {
				entry["parameters"] = tool.Schema
			}
			if tool.Strict {
				entry["strict"] = true
			}
			tools = append(tools, entry)
		}
		payload["tools"] = tools
		if req.ToolChoice != nil {
			payload["tool_choice"] = req.ToolChoice
		}
	}
	if req.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		payload["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	return payload
}

func responsesInput(req *Request) []any {
	var out []any
	for _, message := range req.Messages {
		switch message.Role {
		case "tool":
			for _, part := range message.Parts {
				if part.Type != "tool_result" {
					continue
				}
				out = append(out, map[string]any{
					"type":    "function_call_output",
					"call_id": part.CallID,
					"output":  part.Text,
				})
			}
		case "assistant":
			if text := message.Text(); text != "" {
				out = append(out, map[string]any{
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				})
			}
			for _, part := range message.Parts {
				if part.Type != "tool_call" {
					continue
				}
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   part.CallID,
					"name":      part.Name,
					"arguments": part.Arguments,
				})
			}
		default:
			content := make([]any, 0, len(message.Parts))
			for _, part := range message.Parts {
				switch part.Type {
				case "text":
					content = append(content, map[string]any{"type": "input_text", "text": part.Text})
				case "image":
					content = append(content, map[string]any{"type": "input_image", "image_url": part.ImageURL})
				}
			}
			out = append(out, map[string]any{"role": message.Role, "content": content})
		}
	}
	return out
}

// responsesStreamEvent is one Responses API SSE payload.
type responsesStreamEvent struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	Text   string `json:"text"`
	ItemID string `json:"item_id"`
	Item   *struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	OutputIndex int `json:"output_index"`
	Response    *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
	Message string `json:"message"`
}

// ParseResponsesStream converts a Responses API SSE stream into events.
func ParseResponsesStream(r io.Reader, emit func(Event)) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	toolIndex := map[string]int{}
	nextTool := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					emit(Event{Kind: EventDone})
					return nil
				}
				if data != "" {
					handleResponsesData(data, emit, toolIndex, &nextTool)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				emit(Event{Kind: EventDone})
				return nil
			}
			return err
		}
	}
}

func handleResponsesData(data string, emit func(Event), toolIndex map[string]int, nextTool *int) {
	var event responsesStreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	switch event.Type {
	case "response.output_text.delta":
		emit(Event{Kind: EventText, Text: event.Delta})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		emit(Event{Kind: EventReasoning, Text: event.Delta})
	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "function_call" {
			index := *nextTool
			*nextTool = index + 1
			toolIndex[event.Item.ID] = index
			emit(Event{Kind: EventToolStart, ToolIndex: index, ToolID: event.Item.CallID, ToolName: event.Item.Name})
			if event.Item.Arguments != "" {
				emit(Event{Kind: EventToolArgs, ToolIndex: index, Text: event.Item.Arguments})
			}
		}
	case "response.function_call_arguments.delta":
		if index, ok := toolIndex[event.ItemID]; ok {
			emit(Event{Kind: EventToolArgs, ToolIndex: index, Text: event.Delta})
		}
	case "response.output_item.done":
		if event.Item != nil && event.Item.Type == "function_call" {
			if index, ok := toolIndex[event.Item.ID]; ok {
				emit(Event{Kind: EventToolEnd, ToolIndex: index})
			}
		}
	case "response.completed":
		if event.Response != nil && event.Response.Usage != nil {
			emit(Event{Kind: EventUsage, Usage: &Usage{
				InputTokens:  event.Response.Usage.InputTokens,
				OutputTokens: event.Response.Usage.OutputTokens,
				TotalTokens:  event.Response.Usage.TotalTokens,
			}})
		}
		emit(Event{Kind: EventDone})
	case "response.failed", "error":
		message := event.Message
		if event.Error != nil {
			message = event.Error.Message
			if event.Error.Code != "" {
				message = event.Error.Code + ": " + message
			}
		}
		if message == "" {
			message = "upstream response failed"
		}
		emit(Event{Kind: EventError, Err: fmt.Errorf("%s", message)})
	}
}
