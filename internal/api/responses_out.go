package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ResponsesWriter renders protocol-neutral events as OpenAI Responses API
// server-sent events. With a nil writer it aggregates the same events into a
// single response object instead.
type ResponsesWriter struct {
	w     io.Writer
	flush func()

	ID      string
	Model   string
	Created int64
	seq     int

	started bool
	done    bool
	failed  bool

	items []*outputItem

	msg        *outputItem
	reasoning  *outputItem
	tools      map[int]*outputItem
	toolDrops  map[int]bool
	clientTool map[string]bool
	injected   map[string]bool

	usage        Usage
	finishReason string
	errMessage   string
}

type outputItem struct {
	index     int
	id        string
	kind      string // message | reasoning | function_call
	status    string
	text      strings.Builder
	name      string
	callID    string
	arguments strings.Builder
	added     bool
	partAdded bool
	closed    bool
	dropped   bool
	buffered  []Event
}

// NewResponsesWriter creates a writer. When w is nil the writer only
// aggregates, and Final returns the completed response object.
func NewResponsesWriter(w io.Writer, flush func(), id, model string, clientTools, injected []string) *ResponsesWriter {
	client := make(map[string]bool, len(clientTools))
	for _, name := range clientTools {
		client[name] = true
	}
	inj := make(map[string]bool, len(injected))
	for _, name := range injected {
		inj[name] = true
	}
	return &ResponsesWriter{
		w:          w,
		flush:      flush,
		ID:         id,
		Model:      model,
		Created:    time.Now().Unix(),
		tools:      map[int]*outputItem{},
		toolDrops:  map[int]bool{},
		clientTool: client,
		injected:   inj,
	}
}

// Emit consumes one protocol-neutral event.
func (r *ResponsesWriter) Emit(ev Event) {
	if r.failed {
		return
	}
	switch ev.Kind {
	case EventText:
		if ev.Text == "" {
			return
		}
		r.ensureStarted()
		if r.reasoning != nil && !r.reasoning.closed {
			r.closeItem(r.reasoning)
		}
		item := r.ensureMessage()
		r.emitItemAdded(item)
		r.emitContentPart(item)
		item.text.WriteString(ev.Text)
		r.send("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"delta":         ev.Text,
			"logprobs":      []any{},
		})
	case EventReasoning:
		if ev.Text == "" {
			return
		}
		r.ensureStarted()
		item := r.ensureReasoning()
		r.emitItemAdded(item)
		r.emitSummaryPart(item)
		item.text.WriteString(ev.Text)
		r.send("response.reasoning_summary_text.delta", map[string]any{
			"type":          "response.reasoning_summary_text.delta",
			"item_id":       item.id,
			"output_index":  item.index,
			"summary_index": 0,
			"delta":         ev.Text,
		})
	case EventToolStart:
		r.ensureStarted()
		item, ok := r.tools[ev.ToolIndex]
		if !ok {
			item = r.newToolItem(ev)
			r.tools[ev.ToolIndex] = item
		}
		if item.name == "" && ev.ToolName != "" {
			item.name = ev.ToolName
		}
		if r.isDropped(item.name) {
			r.toolDrops[ev.ToolIndex] = true
			r.dropItem(item)
			return
		}
		if item.name != "" {
			r.emitItemAdded(item)
			for _, pending := range item.buffered {
				r.appendToolArgs(item, pending.Text)
			}
			item.buffered = nil
		}
	case EventToolArgs:
		if r.toolDrops[ev.ToolIndex] {
			return
		}
		item, ok := r.tools[ev.ToolIndex]
		if !ok {
			item = r.newToolItem(ev)
			r.tools[ev.ToolIndex] = item
		}
		if r.isDropped(item.name) {
			r.toolDrops[ev.ToolIndex] = true
			r.dropItem(item)
			return
		}
		if !item.added {
			item.buffered = append(item.buffered, ev)
			return
		}
		r.appendToolArgs(item, ev.Text)
	case EventToolEnd:
		item, ok := r.tools[ev.ToolIndex]
		if !ok || r.toolDrops[ev.ToolIndex] {
			return
		}
		if ev.ToolName != "" {
			item.name = ev.ToolName
		}
		if ev.ToolID != "" {
			item.callID = ev.ToolID
		}
		if r.isDropped(item.name) {
			r.toolDrops[ev.ToolIndex] = true
			r.dropItem(item)
			return
		}
		if !item.added && item.name != "" {
			r.emitItemAdded(item)
			for _, pending := range item.buffered {
				r.appendToolArgs(item, pending.Text)
			}
			item.buffered = nil
		}
		r.closeItem(item)
	case EventUsage:
		if ev.Usage != nil {
			r.usage = *ev.Usage
		}
	case EventDone:
		if ev.FinishReason != "" {
			r.finishReason = ev.FinishReason
		}
		r.Finish("completed")
	case EventError:
		r.Fail(ev.Err)
	}
}

func (r *ResponsesWriter) newToolItem(ev Event) *outputItem {
	if r.msg != nil && !r.msg.closed {
		r.closeItem(r.msg)
	}
	if r.reasoning != nil && !r.reasoning.closed {
		r.closeItem(r.reasoning)
	}
	name := ev.ToolName
	callID := ev.ToolID
	if callID == "" {
		callID = "call_" + randomHex(12)
	}
	item := &outputItem{
		kind:   "function_call",
		id:     "fc_" + randomHex(16),
		name:   name,
		callID: callID,
		status: "in_progress",
		index:  len(r.items),
	}
	r.items = append(r.items, item)
	return item
}

func (r *ResponsesWriter) isDropped(name string) bool {
	if name == "" {
		return false
	}
	if r.clientTool[name] {
		return false
	}
	return r.injected[name]
}

func (r *ResponsesWriter) dropItem(item *outputItem) {
	if item.dropped {
		return
	}
	item.dropped = true
	for i, existing := range r.items {
		if existing == item {
			r.items = append(r.items[:i], r.items[i+1:]...)
			return
		}
	}
}

func (r *ResponsesWriter) appendToolArgs(item *outputItem, delta string) {
	if delta == "" {
		return
	}
	item.arguments.WriteString(delta)
	r.send("response.function_call_arguments.delta", map[string]any{
		"type":         "response.function_call_arguments.delta",
		"item_id":      item.id,
		"output_index": item.index,
		"delta":        delta,
	})
}

func (r *ResponsesWriter) ensureStarted() {
	if r.started {
		return
	}
	r.started = true
	r.send("response.created", map[string]any{
		"type":     "response.created",
		"response": r.responseObject("in_progress"),
	})
	r.send("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": r.responseObject("in_progress"),
	})
}

func (r *ResponsesWriter) ensureMessage() *outputItem {
	if r.msg != nil && !r.msg.closed {
		return r.msg
	}
	item := &outputItem{
		kind:   "message",
		id:     "msg_" + randomHex(16),
		status: "in_progress",
		index:  len(r.items),
	}
	r.items = append(r.items, item)
	r.msg = item
	return item
}

func (r *ResponsesWriter) ensureReasoning() *outputItem {
	if r.reasoning != nil && !r.reasoning.closed {
		return r.reasoning
	}
	item := &outputItem{
		kind:   "reasoning",
		id:     "rs_" + randomHex(16),
		status: "in_progress",
		index:  len(r.items),
	}
	r.items = append(r.items, item)
	r.reasoning = item
	return item
}

func (r *ResponsesWriter) emitItemAdded(item *outputItem) {
	if item.added {
		return
	}
	item.added = true
	r.send("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": item.index,
		"item":         r.itemObject(item, "in_progress"),
	})
}

func (r *ResponsesWriter) emitContentPart(item *outputItem) {
	if item.partAdded {
		return
	}
	item.partAdded = true
	r.send("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       item.id,
		"output_index":  item.index,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (r *ResponsesWriter) emitSummaryPart(item *outputItem) {
	if item.partAdded {
		return
	}
	item.partAdded = true
	r.send("response.reasoning_summary_part.added", map[string]any{
		"type":          "response.reasoning_summary_part.added",
		"item_id":       item.id,
		"output_index":  item.index,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

func (r *ResponsesWriter) closeItem(item *outputItem) {
	if item.closed {
		return
	}
	item.closed = true
	switch item.kind {
	case "message":
		if item.partAdded {
			r.send("response.output_text.done", map[string]any{
				"type":          "response.output_text.done",
				"item_id":       item.id,
				"output_index":  item.index,
				"content_index": 0,
				"text":          item.text.String(),
			})
			r.send("response.content_part.done", map[string]any{
				"type":          "response.content_part.done",
				"item_id":       item.id,
				"output_index":  item.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": item.text.String(), "annotations": []any{}},
			})
		}
	case "reasoning":
		if item.partAdded {
			r.send("response.reasoning_summary_text.done", map[string]any{
				"type":          "response.reasoning_summary_text.done",
				"item_id":       item.id,
				"output_index":  item.index,
				"summary_index": 0,
				"text":          item.text.String(),
			})
			r.send("response.reasoning_summary_part.done", map[string]any{
				"type":          "response.reasoning_summary_part.done",
				"item_id":       item.id,
				"output_index":  item.index,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": item.text.String()},
			})
		}
	case "function_call":
		r.send("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      item.id,
			"output_index": item.index,
			"arguments":    item.arguments.String(),
		})
	}
	r.send("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": item.index,
		"item":         r.itemObject(item, "completed"),
	})
}

func (r *ResponsesWriter) itemObject(item *outputItem, status string) map[string]any {
	switch item.kind {
	case "message":
		return map[string]any{
			"id":      item.id,
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": item.text.String(), "annotations": []any{}}},
		}
	case "reasoning":
		return map[string]any{
			"id":      item.id,
			"type":    "reasoning",
			"status":  status,
			"summary": []any{map[string]any{"type": "summary_text", "text": item.text.String()}},
		}
	default:
		return map[string]any{
			"id":        item.id,
			"type":      "function_call",
			"status":    status,
			"name":      item.name,
			"arguments": item.arguments.String(),
			"call_id":   item.callID,
		}
	}
}

// Finish closes open items and emits response.completed.
func (r *ResponsesWriter) Finish(status string) {
	if r.done || r.failed {
		return
	}
	r.ensureStarted()
	for _, item := range r.items {
		r.closeItem(item)
	}
	r.done = true
	r.send("response.completed", map[string]any{
		"type":     "response.completed",
		"response": r.responseObject(status),
	})
	if r.w != nil {
		fmt.Fprint(r.w, "data: [DONE]\n\n")
		if r.flush != nil {
			r.flush()
		}
	}
}

// Fail reports a terminal error to the client.
func (r *ResponsesWriter) Fail(err error) {
	if r.done || r.failed {
		return
	}
	r.failed = true
	message := "upstream error"
	if err != nil {
		message = err.Error()
	}
	r.errMessage = message
	r.send("error", map[string]any{
		"type":    "error",
		"code":    "upstream_error",
		"message": message,
	})
	if r.w != nil {
		fmt.Fprint(r.w, "data: [DONE]\n\n")
		if r.flush != nil {
			r.flush()
		}
	}
}

func (r *ResponsesWriter) responseObject(status string) map[string]any {
	output := make([]any, 0, len(r.items))
	for _, item := range r.items {
		if item.dropped {
			continue
		}
		output = append(output, r.itemObject(item, "completed"))
	}
	obj := map[string]any{
		"id":                  r.ID,
		"object":              "response",
		"created_at":          r.Created,
		"status":              status,
		"model":               r.Model,
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"error":               nil,
		"incomplete_details":  nil,
		"usage":               r.usageObject(),
	}
	if r.finishReason == "length" && status == "completed" {
		obj["status"] = "incomplete"
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if r.errMessage != "" {
		obj["error"] = map[string]any{"code": "upstream_error", "message": r.errMessage}
	}
	return obj
}

func (r *ResponsesWriter) usageObject() map[string]any {
	total := r.usage.TotalTokens
	if total == 0 {
		total = r.usage.InputTokens + r.usage.OutputTokens
	}
	usage := map[string]any{
		"input_tokens":          r.usage.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": r.usage.CachedTokens},
		"output_tokens":         r.usage.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": r.usage.Reasoning},
		"total_tokens":          total,
	}
	return usage
}

// Final returns the aggregated response object for non-streaming clients.
func (r *ResponsesWriter) Final() map[string]any {
	if !r.started {
		r.ensureStarted()
	}
	status := "completed"
	if r.errMessage != "" {
		status = "failed"
	}
	return r.responseObject(status)
}

// Failed reports whether a terminal error was recorded.
func (r *ResponsesWriter) Failed() bool { return r.failed }

// ErrorMessage returns the recorded terminal error text.
func (r *ResponsesWriter) ErrorMessage() string { return r.errMessage }

func (r *ResponsesWriter) send(event string, payload map[string]any) {
	if r.w == nil {
		return
	}
	r.seq++
	payload["sequence_number"] = r.seq
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(r.w, "event: %s\ndata: %s\n\n", event, encoded)
	if r.flush != nil {
		r.flush()
	}
}
