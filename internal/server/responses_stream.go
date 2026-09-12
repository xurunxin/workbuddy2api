package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type streamToolState struct {
	itemID    string
	callID    string
	name      string
	arguments strings.Builder
	outputIdx int
	added     bool
}

// responsesStreamWriter consumes the normalized Chat Completions SSE frames
// written by upstream.Stream and emits Responses API semantic events as each
// delta arrives. It intentionally implements http.Flusher so upstream.Stream
// retains its per-frame flush behavior all the way to the client.
type responsesStreamWriter struct {
	dst          http.ResponseWriter
	header       http.Header
	base         map[string]any
	onFinal      func(map[string]any, []any)
	buf          bytes.Buffer
	sequence     int
	started      bool
	done         bool
	failed       bool
	status       int
	writeErr     error
	finish       string
	messageID    string
	messageIdx   int
	messageOn    bool
	textOn       bool
	text         strings.Builder
	tools        map[int]*streamToolState
	toolOrder    []int
	usage        any
	reasoningID  string
	reasoningIdx int
	reasoning    strings.Builder
}

func newResponsesStreamWriter(dst http.ResponseWriter, base map[string]any, onFinal func(map[string]any, []any)) *responsesStreamWriter {
	return &responsesStreamWriter{
		dst: dst, header: make(http.Header), base: cloneMap(base), onFinal: onFinal,
		status: http.StatusOK, messageID: newResponseID("msg"), messageIdx: -1,
		tools: make(map[int]*streamToolState),
	}
}

func (w *responsesStreamWriter) Header() http.Header { return w.header }

func (w *responsesStreamWriter) WriteHeader(status int) {
	if w.started || w.status != http.StatusOK {
		return
	}
	w.status = status
	if status != http.StatusOK {
		copyHeader(w.dst.Header(), w.header)
		w.dst.WriteHeader(status)
		return
	}
	w.start()
}

func (w *responsesStreamWriter) start() {
	if w.started {
		return
	}
	w.started = true
	h := w.dst.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.dst.WriteHeader(http.StatusOK)
	created := cloneMap(w.base)
	created["status"] = "in_progress"
	w.emit("response.created", map[string]any{"response": created})
	w.emit("response.in_progress", map[string]any{"response": created})
}

func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	if w.status != http.StatusOK {
		if len(p) == 0 {
			return 0, nil
		}
		return w.dst.Write(p)
	}
	if !w.started {
		w.start()
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	_, _ = w.buf.Write(p)
	for {
		raw := w.buf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
		sep := 2
		if idx < 0 {
			idx = bytes.Index(raw, []byte("\r\n\r\n"))
			sep = 4
		}
		if idx < 0 {
			break
		}
		frame := string(append([]byte(nil), raw[:idx]...))
		w.buf.Next(idx + sep)
		w.consumeFrame(frame)
		if w.writeErr != nil {
			return 0, w.writeErr
		}
	}
	return len(p), nil
}

func (w *responsesStreamWriter) Flush() {
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesStreamWriter) consumeFrame(frame string) {
	if w.done {
		return
	}
	var payload string
	for _, line := range strings.Split(strings.ReplaceAll(frame, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		w.complete()
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		w.fail("upstream_parse", "Invalid JSON in upstream stream.")
		return
	}
	if upstreamErr, ok := chunk["error"].(map[string]any); ok {
		code, _ := upstreamErr["code"].(string)
		message, _ := upstreamErr["message"].(string)
		if code == "" {
			code = "upstream_error"
		}
		if message == "" {
			message = "Upstream stream failed."
		}
		w.fail(code, message)
		return
	}
	if rawUsage, ok := chunk["usage"]; ok && rawUsage != nil {
		w.usage = responseUsage(rawUsage)
	}
	choices, _ := chunk["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			w.finish = reason
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if text, _ := delta["reasoning_content"].(string); text != "" {
			w.reasoningDelta(text)
		}
		if text, ok := delta["content"].(string); ok && text != "" {
			w.textDelta(text)
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, rawCall := range rawCalls {
				call, _ := rawCall.(map[string]any)
				if call != nil {
					w.toolDelta(call)
				}
			}
		}
	}
}

func (w *responsesStreamWriter) reasoningDelta(delta string) {
	if w.reasoningID == "" {
		w.reasoningID = newResponseID("rs")
		w.reasoningIdx = len(w.currentOutput())
		item := responseReasoningItem(w.reasoningID, "in_progress", "")
		item["summary"] = []any{}
		w.appendOutput(item)
		w.emit("response.output_item.added", map[string]any{"output_index": w.reasoningIdx, "item": cloneMap(item)})
		w.emit("response.reasoning_summary_part.added", map[string]any{"item_id": w.reasoningID, "output_index": w.reasoningIdx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	}
	w.reasoning.WriteString(delta)
	w.currentOutput()[w.reasoningIdx] = responseReasoningItem(w.reasoningID, "in_progress", w.reasoning.String())
	w.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": w.reasoningID, "output_index": w.reasoningIdx, "summary_index": 0, "delta": delta})
}

func (w *responsesStreamWriter) ensureMessage() {
	if w.messageOn {
		return
	}
	w.messageOn = true
	w.messageIdx = len(w.currentOutput())
	item := map[string]any{"id": w.messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
	w.appendOutput(item)
	w.emit("response.output_item.added", map[string]any{"output_index": w.messageIdx, "item": cloneMap(item)})
}

func (w *responsesStreamWriter) textDelta(delta string) {
	w.ensureMessage()
	if !w.textOn {
		w.textOn = true
		part := map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""}
		w.emit("response.content_part.added", map[string]any{"item_id": w.messageID, "output_index": w.messageIdx, "content_index": 0, "part": part})
	}
	w.text.WriteString(delta)
	w.emit("response.output_text.delta", map[string]any{
		"item_id": w.messageID, "output_index": w.messageIdx, "content_index": 0, "delta": delta, "logprobs": []any{},
	})
}

func (w *responsesStreamWriter) toolDelta(call map[string]any) {
	idx := int(numericToFloat(call["index"]))
	state, ok := w.tools[idx]
	if !ok {
		state = &streamToolState{outputIdx: -1}
		w.tools[idx] = state
		w.toolOrder = append(w.toolOrder, idx)
	}
	if id, _ := call["id"].(string); id != "" && !state.added {
		state.callID = id
	}
	fn, _ := call["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != "" && !state.added {
		state.name = name
	}
	args, _ := fn["arguments"].(string)
	if args != "" {
		state.arguments.WriteString(args)
	}
	// A valid Responses function item must have stable call_id and name in
	// output_item.added. Some upstreams split arguments before identity fields;
	// buffer those fragments until both identifiers arrive.
	if !state.added && state.callID != "" && state.name != "" {
		state.itemID = functionItemID(state.callID)
		state.outputIdx = len(w.currentOutput())
		state.added = true
		item := responseFunctionItem(state.itemID, state.callID, state.name, "", "in_progress")
		w.appendOutput(item)
		w.emit("response.output_item.added", map[string]any{"output_index": state.outputIdx, "item": cloneMap(item)})
		if buffered := state.arguments.String(); buffered != "" {
			w.emit("response.function_call_arguments.delta", map[string]any{"item_id": state.itemID, "output_index": state.outputIdx, "delta": buffered})
		}
		return
	}
	if state.added && args != "" {
		w.emit("response.function_call_arguments.delta", map[string]any{"item_id": state.itemID, "output_index": state.outputIdx, "delta": args})
	}
}

func (w *responsesStreamWriter) complete() {
	if w.done {
		return
	}
	if w.failed {
		w.done = true
		return
	}
	output := w.currentOutput()
	generated := []any{}
	if w.reasoningID != "" {
		text := w.reasoning.String()
		w.emit("response.reasoning_summary_text.done", map[string]any{"item_id": w.reasoningID, "output_index": w.reasoningIdx, "summary_index": 0, "text": text})
		w.emit("response.reasoning_summary_part.done", map[string]any{"item_id": w.reasoningID, "output_index": w.reasoningIdx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		output[w.reasoningIdx] = responseReasoningItem(w.reasoningID, "completed", text)
		w.emit("response.output_item.done", map[string]any{"output_index": w.reasoningIdx, "item": output[w.reasoningIdx]})
	}
	if w.messageOn {
		text := w.text.String()
		if w.textOn {
			w.emit("response.output_text.done", map[string]any{"item_id": w.messageID, "output_index": w.messageIdx, "content_index": 0, "text": text, "logprobs": []any{}})
			part := map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text}
			w.emit("response.content_part.done", map[string]any{"item_id": w.messageID, "output_index": w.messageIdx, "content_index": 0, "part": part})
			output[w.messageIdx] = responseMessageItem(w.messageID, "completed", text)
			generated = append(generated, map[string]any{"role": "assistant", "content": text})
		}
		w.emit("response.output_item.done", map[string]any{"output_index": w.messageIdx, "item": output[w.messageIdx]})
	}
	toolCalls := []any{}
	for _, idx := range w.toolOrder {
		state := w.tools[idx]
		if state == nil {
			continue
		}
		if !state.added {
			// A missing call ID can safely receive a gateway ID once the function
			// name is known. Without a name there is no valid function item to
			// expose; leave the response incomplete rather than emit a malformed
			// output item.
			if state.name == "" {
				w.finish = ""
				continue
			}
			if state.callID == "" {
				state.callID = newResponseID("call")
			}
			state.itemID = functionItemID(state.callID)
			state.outputIdx = len(w.currentOutput())
			state.added = true
			item := responseFunctionItem(state.itemID, state.callID, state.name, "", "in_progress")
			w.appendOutput(item)
			output = w.currentOutput()
			w.emit("response.output_item.added", map[string]any{"output_index": state.outputIdx, "item": cloneMap(item)})
			if buffered := state.arguments.String(); buffered != "" {
				w.emit("response.function_call_arguments.delta", map[string]any{"item_id": state.itemID, "output_index": state.outputIdx, "delta": buffered})
			}
		}
		args := state.arguments.String()
		w.emit("response.function_call_arguments.done", map[string]any{"item_id": state.itemID, "output_index": state.outputIdx, "arguments": args})
		item := responseFunctionItem(state.itemID, state.callID, state.name, args, "completed")
		output[state.outputIdx] = item
		w.emit("response.output_item.done", map[string]any{"output_index": state.outputIdx, "item": item})
		toolCalls = append(toolCalls, map[string]any{"id": state.callID, "type": "function", "function": map[string]any{"name": state.name, "arguments": args}})
	}
	if len(toolCalls) > 0 {
		generated = append(generated, map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls})
	}
	generated = preserveResponseReasoning(generated, w.reasoning.String())
	final := cloneMap(w.base)
	final["output"] = output
	final["usage"] = w.usage
	event := "response.completed"
	status := "completed"
	if w.finish == "length" || w.finish == "content_filter" || w.finish == "" {
		status = "incomplete"
		event = "response.incomplete"
		reason := w.finish
		if reason == "length" {
			reason = "max_output_tokens"
		}
		if reason == "" {
			reason = "upstream_incomplete"
		}
		final["incomplete_details"] = map[string]any{"reason": reason}
	}
	final["status"] = status
	w.emit(event, map[string]any{"response": final})
	w.done = true
	if w.onFinal != nil {
		w.onFinal(final, generated)
	}
}

func (w *responsesStreamWriter) fail(code, message string) {
	if w.done || w.failed {
		return
	}
	w.failed = true
	errObj := map[string]any{"code": code, "message": message, "param": nil, "type": "server_error"}
	w.emit("error", errObj)
	final := cloneMap(w.base)
	final["status"] = "failed"
	final["error"] = errObj
	final["output"] = w.currentOutput()
	w.emit("response.failed", map[string]any{"response": final})
	w.done = true
}

func (w *responsesStreamWriter) finishIfNeeded() {
	if w.status != http.StatusOK || w.done {
		return
	}
	if !w.started {
		w.start()
	}
	// Chat's SSE adapter only omits [DONE] when the upstream reader or client
	// write failed. Preserve partial output and mark the response incomplete.
	w.complete()
}

func (w *responsesStreamWriter) emit(event string, payload map[string]any) {
	if w.writeErr != nil {
		return
	}
	payload["type"] = event
	payload["sequence_number"] = w.sequence
	w.sequence++
	raw, err := json.Marshal(payload)
	if err != nil {
		w.writeErr = err
		return
	}
	_, err = fmt.Fprintf(w.dst, "event: %s\ndata: %s\n\n", event, raw)
	if err != nil {
		w.writeErr = err
		return
	}
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesStreamWriter) currentOutput() []any {
	output, _ := w.base["output"].([]any)
	return output
}

func (w *responsesStreamWriter) appendOutput(item any) {
	output := w.currentOutput()
	output = append(output, item)
	w.base["output"] = output
}

var _ http.ResponseWriter = (*responsesStreamWriter)(nil)
var _ http.Flusher = (*responsesStreamWriter)(nil)
var _ = io.EOF
