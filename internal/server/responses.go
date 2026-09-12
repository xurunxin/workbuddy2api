package server

// This file implements the stateful subset of OpenAI's Responses API on top
// of the gateway's existing Chat Completions execution path. Keeping execution
// in chatCompletions preserves account rotation, cooldowns, prompt rewriting,
// request logging, and session affinity in one place.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	responseStoreTTL      = 30 * time.Minute
	responseStoreMaxItems = 1000
	responseStoreMaxBytes = 64 << 20
)

type responseCreateRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions"`
	Tools              []json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	Reasoning          json.RawMessage   `json:"reasoning"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	Stream             bool              `json:"stream"`
	Store              *bool             `json:"store"`
	PreviousResponseID string            `json:"previous_response_id"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	Metadata           map[string]any    `json:"metadata"`
	User               string            `json:"user"`
	ServiceTier        string            `json:"service_tier"`
}

type storedResponse struct {
	response     map[string]any
	history      []any
	conversation string
	created      time.Time
	expires      time.Time
	bytes        int
}

type responseStore struct {
	mu    sync.Mutex
	items map[string]storedResponse
	bytes int
	now   func() time.Time
}

func newResponseStore() *responseStore {
	return &responseStore{items: make(map[string]storedResponse), now: time.Now}
}

func (s *responseStore) get(id string) (storedResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	v, ok := s.items[id]
	if !ok {
		return storedResponse{}, false
	}
	v.response = cloneMap(v.response)
	v.history = cloneSlice(v.history)
	return v, true
}

func (s *responseStore) put(id string, response map[string]any, history []any, conversation string) {
	response = cloneMap(response)
	history = cloneSlice(history)
	rawResponse, _ := json.Marshal(response)
	rawHistory, _ := json.Marshal(history)
	sz := len(rawResponse) + len(rawHistory)
	// A single oversized conversation is not retained; the response was still
	// delivered successfully, but cannot displace the entire bounded store.
	if sz > responseStoreMaxBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if old, ok := s.items[id]; ok {
		s.bytes -= old.bytes
	}
	now := s.now()
	s.items[id] = storedResponse{response: response, history: history, conversation: conversation, created: now, expires: now.Add(responseStoreTTL), bytes: sz}
	s.bytes += sz
	for len(s.items) > responseStoreMaxItems || s.bytes > responseStoreMaxBytes {
		oldestID := ""
		var oldest time.Time
		for candidate, item := range s.items {
			if oldestID == "" || item.created.Before(oldest) {
				oldestID, oldest = candidate, item.created
			}
		}
		if oldestID == "" {
			break
		}
		s.bytes -= s.items[oldestID].bytes
		delete(s.items, oldestID)
	}
}

func (s *responseStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	v, ok := s.items[id]
	if ok {
		s.bytes -= v.bytes
		delete(s.items, id)
	}
	return ok
}

func (s *responseStore) pruneLocked() {
	now := s.now()
	for id, item := range s.items {
		if !now.Before(item.expires) {
			s.bytes -= item.bytes
			delete(s.items, id)
		}
	}
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	raw, _ := json.Marshal(src)
	var dst map[string]any
	_ = json.Unmarshal(raw, &dst)
	return dst
}

func cloneSlice(src []any) []any {
	if src == nil {
		return nil
	}
	raw, _ := json.Marshal(src)
	var dst []any
	_ = json.Unmarshal(raw, &dst)
	return dst
}

func (h *Handler) getResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("response_id")
	item, ok := h.responses.get(id)
	if !ok {
		writeResponsesError(w, http.StatusNotFound, "response_not_found", "No stored response found with id '"+id+"'.", "response_id")
		return
	}
	writeJSON(w, http.StatusOK, item.response)
}

func (h *Handler) deleteResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("response_id")
	if !h.responses.delete(id) {
		writeResponsesError(w, http.StatusNotFound, "response_not_found", "No stored response found with id '"+id+"'.", "response_id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true})
}

func (h *Handler) createResponse(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error(), nil)
		return
	}
	if int64(len(body)) > limit {
		writeResponsesError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("request body exceeds the configured %d MB limit", limit>>20), nil)
		return
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_json", "Could not parse the request body as JSON.", nil)
		return
	}
	if err := validateResponseFields(fields); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "unsupported_parameter", err.Error(), responseErrorParam(err))
		return
	}
	var req responseCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeResponsesError(w, http.StatusBadRequest, "missing_required_parameter", "Missing required parameter: 'model'.", "model")
		return
	}
	if len(req.Input) == 0 || bytes.Equal(bytes.TrimSpace(req.Input), []byte("null")) {
		writeResponsesError(w, http.StatusBadRequest, "missing_required_parameter", "Missing required parameter: 'input'.", "input")
		return
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens <= 0 {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "'max_output_tokens' must be greater than zero.", "max_output_tokens")
		return
	}

	store := req.Store == nil || *req.Store
	id := newResponseID("resp")
	conversationID := id
	var priorHistory []any
	if req.PreviousResponseID != "" {
		prior, ok := h.responses.get(req.PreviousResponseID)
		if !ok {
			writeResponsesError(w, http.StatusNotFound, "response_not_found", "No stored response found with id '"+req.PreviousResponseID+"'.", "previous_response_id")
			return
		}
		priorHistory = prior.history
		if prior.conversation != "" {
			conversationID = prior.conversation
		} else {
			conversationID = req.PreviousResponseID
		}
	}
	currentMessages, err := responseInputToMessages(req.Input)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_input", err.Error(), "input")
		return
	}
	historyMessages := append(cloneSlice(priorHistory), currentMessages...)
	messages := cloneSlice(historyMessages)
	if req.Instructions != "" {
		messages = append([]any{map[string]any{"role": "developer", "content": req.Instructions}}, messages...)
	}
	chatBody, originalTools, originalToolChoice, reasoningEffort, err := responseToChatBody(req, messages, conversationID)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "unsupported_parameter", err.Error(), responseErrorParam(err))
		return
	}
	if int64(len(chatBody)) > h.cfg.MaxBodyBytes {
		writeResponsesError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "Expanded conversation exceeds the configured request body limit.", "previous_response_id")
		return
	}

	created := time.Now().Unix()
	base := newResponseObject(id, created, req, originalTools, originalToolChoice, reasoningEffort)
	chatReq := r.Clone(r.Context())
	chatReq.URL.Path = "/v1/chat/completions"
	chatReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	chatReq.ContentLength = int64(len(chatBody))

	if req.Stream {
		sw := newResponsesStreamWriter(w, base, func(final map[string]any, generated []any) {
			if store {
				h.responses.put(id, final, append(historyMessages, generated...), conversationID)
			}
		})
		h.chatCompletions(sw, chatReq)
		sw.finishIfNeeded()
		return
	}

	capture := newCaptureResponseWriter()
	h.chatCompletions(capture, chatReq)
	if capture.status != http.StatusOK {
		copyHeader(w.Header(), capture.header)
		w.WriteHeader(capture.status)
		_, _ = w.Write(capture.body.Bytes())
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(capture.body.Bytes(), &chat); err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_parse", "Could not convert the upstream response.", nil)
		return
	}
	final, generated, err := chatResponseToResponse(base, chat)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_parse", err.Error(), nil)
		return
	}
	if store {
		h.responses.put(id, final, append(historyMessages, generated...), conversationID)
	}
	writeJSON(w, http.StatusOK, final)
}

type responseParameterError struct {
	param string
	error
}

func responseErrorParam(err error) any {
	var parameterError *responseParameterError
	if errors.As(err, &parameterError) && parameterError.param != "" {
		return parameterError.param
	}
	return nil
}

func validateResponseFields(fields map[string]json.RawMessage) (err error) {
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "tools": true,
		"tool_choice": true, "reasoning": true, "max_output_tokens": true,
		"stream": true, "store": true, "previous_response_id": true,
		"temperature": true, "top_p": true, "parallel_tool_calls": true,
		"metadata": true, "user": true, "service_tier": true,
		"background": true, "include": true, "text": true, "truncation": true,
		"stream_options": true, "prompt": true, "prompt_cache_key": true,
		"prompt_cache_retention": true, "safety_identifier": true, "max_tool_calls": true,
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Only record known parameter names, never arbitrary client-provided keys.
	parameter := ""
	defer func() {
		if err != nil {
			err = &responseParameterError{param: parameter, error: err}
		}
	}()
	for _, key := range keys {
		raw := fields[key]
		parameter = ""
		if !allowed[key] {
			return fmt.Errorf("Unsupported parameter: '%s'.", key)
		}
		parameter = key
		trimmed := bytes.TrimSpace(raw)
		if bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		switch key {
		case "background":
			if string(trimmed) != "false" {
				return fmt.Errorf("'background' mode is not supported by this gateway")
			}
		case "include":
			var includes []string
			if json.Unmarshal(raw, &includes) != nil {
				return fmt.Errorf("'include' must be an array of strings")
			}
			for _, include := range includes {
				// This is a request for optional output data. CodeBuddy does not
				// provide encrypted reasoning; omit it rather than inventing it.
				if include != "reasoning.encrypted_content" && include != "message.input_image.image_url" {
					return fmt.Errorf("requested 'include' expansion is not supported by this gateway")
				}
			}
		case "text":
			var textOpt struct {
				Format    json.RawMessage `json:"format"`
				Verbosity string          `json:"verbosity"`
			}
			if decodeResponseOption(raw, &textOpt) != nil || !oneOf(textOpt.Verbosity, "", "low", "medium", "high") {
				return fmt.Errorf("'text.verbosity' must be low, medium or high")
			}
			if len(textOpt.Format) > 0 && !bytes.Equal(bytes.TrimSpace(textOpt.Format), []byte("null")) {
				var format struct {
					Type string `json:"type"`
				}
				if decodeResponseOption(textOpt.Format, &format) != nil || format.Type != "text" {
					return fmt.Errorf("structured 'text.format' is not supported by this gateway")
				}
			}
		case "truncation":
			var value string
			if json.Unmarshal(raw, &value) != nil || (value != "" && value != "disabled") {
				return fmt.Errorf("only truncation='disabled' is supported by this gateway")
			}
		case "stream_options":
			var options struct {
				IncludeObfuscation *bool `json:"include_obfuscation"`
			}
			if decodeResponseOption(raw, &options) != nil {
				return fmt.Errorf("'stream_options' supports only a boolean include_obfuscation")
			}
		case "prompt_cache_key":
			var value string
			if json.Unmarshal(raw, &value) != nil || len([]rune(value)) > 64 {
				return fmt.Errorf("'prompt_cache_key' must be a string of at most 64 characters")
			}
		case "prompt_cache_retention":
			var value string
			if json.Unmarshal(raw, &value) != nil || !oneOf(value, "in-memory", "24h") {
				return fmt.Errorf("'prompt_cache_retention' must be in-memory or 24h")
			}
			// Cache preferences are accepted for client compatibility. CodeBuddy
			// controls its own prompt cache and exposes no corresponding options.
		case "prompt", "safety_identifier", "max_tool_calls":
			return fmt.Errorf("'%s' is not supported by this gateway", key)
		}
	}
	return nil
}

func decodeResponseOption(raw json.RawMessage, dst any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func responseInputToMessages(raw json.RawMessage) ([]any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("'input' must be a string or an array of input items")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("'input' array must contain at least one item")
	}
	messages := make([]any, 0, len(items))
	pendingReasoning := ""
	var pendingMedia []any
	flushMedia := func() {
		if len(pendingMedia) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": pendingMedia})
			pendingMedia = nil
		}
	}
	for i, itemRaw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			return nil, fmt.Errorf("input[%d] must be an object", i)
		}
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ != "function_call_output" && typ != "reasoning" {
			flushMedia()
		}
		switch typ {
		case "", "message":
			var role string
			if err := json.Unmarshal(item["role"], &role); err != nil || role == "" {
				return nil, fmt.Errorf("input[%d].role is required for a message", i)
			}
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return nil, fmt.Errorf("input[%d].role '%s' is not supported", i, role)
			}
			content, err := responseContentToChat(item["content"], role)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", i, err)
			}
			message := map[string]any{"role": role, "content": content}
			if role == "assistant" && pendingReasoning != "" {
				message["reasoning_content"] = pendingReasoning
			}
			pendingReasoning = ""
			messages = append(messages, message)
		case "reasoning":
			var reasoning struct {
				Summary []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(itemRaw, &reasoning); err != nil {
				return nil, fmt.Errorf("input[%d] reasoning summary is invalid", i)
			}
			for _, part := range reasoning.Summary {
				if part.Type != "summary_text" {
					return nil, fmt.Errorf("input[%d] reasoning summary type is unsupported", i)
				}
				pendingReasoning += part.Text
			}
		case "function_call":
			var call struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(itemRaw, &call); err != nil || call.CallID == "" || call.Name == "" {
				return nil, fmt.Errorf("input[%d] function_call requires call_id and name", i)
			}
			toolCall := map[string]any{"id": call.CallID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}
			// Responses represents parallel calls as sibling output items. Chat
			// Completions represents them in one assistant message. Merge adjacent
			// calls (and calls following an assistant output_text message) so all
			// call IDs remain available to the following tool messages.
			merged := false
			if len(messages) > 0 {
				if previous, ok := messages[len(messages)-1].(map[string]any); ok && previous["role"] == "assistant" {
					previous["tool_calls"] = append(anySlice(previous["tool_calls"]), toolCall)
					if _, exists := previous["content"]; !exists {
						previous["content"] = nil
					}
					merged = true
				}
			}
			if !merged {
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{toolCall}})
			}
			if pendingReasoning != "" {
				messages[len(messages)-1].(map[string]any)["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
		case "function_call_output":
			pendingReasoning = ""
			var out struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(itemRaw, &out); err != nil || out.CallID == "" {
				return nil, fmt.Errorf("input[%d] function_call_output requires call_id", i)
			}
			output, media, err := responseFunctionOutputToChat(out.Output)
			if err != nil {
				return nil, fmt.Errorf("input[%d] function_call_output.output: %w", i, err)
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": out.CallID, "content": output})
			if len(media) > 0 {
				pendingMedia = append(pendingMedia, map[string]any{"type": "text", "text": "Tool output for call_id " + out.CallID + " (external tool data):"})
				pendingMedia = append(pendingMedia, media...)
			}
		default:
			return nil, fmt.Errorf("input[%d].type '%s' is not supported", i, typ)
		}
	}
	flushMedia()
	if pendingReasoning != "" {
		messages = append(messages, map[string]any{"role": "assistant", "content": nil, "reasoning_content": pendingReasoning})
	}
	return messages, nil
}

// Chat tool messages cannot contain images. Return multimodal content separately
// so the caller can append it after the contiguous batch of tool results.
func responseFunctionOutputToChat(raw json.RawMessage) (string, []any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", nil, fmt.Errorf("must be a string or a content array")
	}
	if text, ok := value.(string); ok {
		return text, nil, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return "", nil, fmt.Errorf("must be a string or a content array")
	}
	texts := make([]string, 0, len(parts))
	multimodal := false
	for i, value := range parts {
		part, ok := value.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("content[%d] must be an object", i)
		}
		if part["type"] != "input_text" {
			if part["type"] != "input_image" && part["type"] != "input_file" {
				return "", nil, fmt.Errorf("content[%d] type is not supported", i)
			}
			multimodal = true
			continue
		}
		text, ok := part["text"].(string)
		if !ok {
			return "", nil, fmt.Errorf("content[%d].text must be a string", i)
		}
		texts = append(texts, text)
	}
	if multimodal {
		content, err := responseContentToChat(raw, "user")
		if err != nil {
			return "", nil, err
		}
		return "Multimodal tool output is attached in the following user message, labeled with this tool_call_id.", content.([]any), nil
	}
	return strings.Join(texts, "\n"), nil, nil
}

func responseContentToChat(raw json.RawMessage, role string) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("message content must be a string or an array")
	}
	out := make([]any, 0, len(parts))
	for j, part := range parts {
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "input_text", "output_text":
			var value string
			if err := json.Unmarshal(part["text"], &value); err != nil {
				return nil, fmt.Errorf("content[%d].text must be a string", j)
			}
			out = append(out, map[string]any{"type": "text", "text": value})
		case "input_image":
			if role != "user" {
				return nil, fmt.Errorf("content[%d] input_image is only supported for user messages", j)
			}
			if _, hasFile := part["file_id"]; hasFile {
				return nil, fmt.Errorf("content[%d] file_id image inputs are not supported; use image_url", j)
			}
			var url, detail string
			if err := json.Unmarshal(part["image_url"], &url); err != nil || url == "" {
				return nil, fmt.Errorf("content[%d].image_url must be a non-empty string", j)
			}
			if err := validateMediaURL(url, "image"); err != nil {
				return nil, fmt.Errorf("content[%d]: %w", j, err)
			}
			_ = json.Unmarshal(part["detail"], &detail)
			image := map[string]any{"url": url}
			if detail != "" {
				image["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		case "input_file":
			if role != "user" {
				return nil, fmt.Errorf("content[%d] input_file is only supported for user messages", j)
			}
			if _, ok := part["file_id"]; ok {
				return nil, fmt.Errorf("content[%d] file_id is not supported; use filename and file_data", j)
			}
			if _, ok := part["file_url"]; ok {
				return nil, fmt.Errorf("content[%d] file_url is not supported by Chat upstream; use filename and file_data", j)
			}
			var filename, data string
			if json.Unmarshal(part["filename"], &filename) != nil || filename == "" || json.Unmarshal(part["file_data"], &data) != nil || data == "" {
				return nil, fmt.Errorf("content[%d] input_file requires non-empty filename and file_data strings", j)
			}
			out = append(out, map[string]any{"type": "file", "file": map[string]any{"filename": filename, "file_data": data}})
		default:
			return nil, fmt.Errorf("content[%d].type '%s' is not supported", j, typ)
		}
	}
	return out, nil
}

func responseToChatBody(req responseCreateRequest, messages []any, conversationID string) ([]byte, []any, any, string, error) {
	chat := map[string]any{"model": req.Model, "messages": messages, "stream": req.Stream}
	if req.MaxOutputTokens != nil {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	metadata := cloneMap(req.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if _, ok := metadata["conversation_id"]; !ok {
		if _, ok := metadata["conversationId"]; !ok {
			metadata["conversation_id"] = conversationID
		}
	}
	chat["metadata"] = metadata
	if req.User != "" {
		chat["user"] = req.User
	}
	if req.ServiceTier != "" {
		chat["service_tier"] = req.ServiceTier
	}

	originalTools := make([]any, 0, len(req.Tools))
	chatTools := make([]any, 0, len(req.Tools))
	for i, raw := range req.Tools {
		var tool map[string]any
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, nil, nil, "", fmt.Errorf("tools[%d] must be an object", i)
		}
		typ, _ := tool["type"].(string)
		if typ != "function" {
			return nil, nil, nil, "", fmt.Errorf("tools[%d].type '%s' is not supported; only function tools are available", i, typ)
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, nil, nil, "", fmt.Errorf("tools[%d].name is required", i)
		}
		fn := map[string]any{"name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				fn[key] = value
			}
		}
		chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
		originalTools = append(originalTools, tool)
	}
	if len(chatTools) > 0 {
		chat["tools"] = chatTools
	}

	var originalToolChoice any = "auto"
	if len(req.ToolChoice) > 0 && !bytes.Equal(bytes.TrimSpace(req.ToolChoice), []byte("null")) {
		if err := json.Unmarshal(req.ToolChoice, &originalToolChoice); err != nil {
			return nil, nil, nil, "", fmt.Errorf("tool_choice is invalid")
		}
		switch choice := originalToolChoice.(type) {
		case string:
			if choice != "none" && choice != "auto" && choice != "required" {
				return nil, nil, nil, "", fmt.Errorf("tool_choice '%s' is not supported", choice)
			}
			chat["tool_choice"] = choice
		case map[string]any:
			typ, _ := choice["type"].(string)
			name, _ := choice["name"].(string)
			if typ != "function" || name == "" {
				return nil, nil, nil, "", fmt.Errorf("only named function objects are supported for tool_choice")
			}
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		default:
			return nil, nil, nil, "", fmt.Errorf("tool_choice must be a string or function object")
		}
	}

	reasoningEffort := ""
	if len(req.Reasoning) > 0 && !bytes.Equal(bytes.TrimSpace(req.Reasoning), []byte("null")) {
		var reasoning struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		}
		// Harness can request max, which the shared Chat execution path already
		// supports and adapts using the upstream model's supported effort list.
		if err := decodeResponseOption(req.Reasoning, &reasoning); err != nil || !oneOf(reasoning.Summary, "", "auto", "concise", "detailed") || !oneOf(reasoning.Effort, "", "none", "minimal", "low", "medium", "high", "xhigh", "max") {
			return nil, nil, nil, "", &responseParameterError{param: "reasoning", error: fmt.Errorf("reasoning must contain a valid effort (none, minimal, low, medium, high, xhigh or max) and optional summary (auto, concise or detailed)")}
		}
		reasoningEffort = reasoning.Effort
		if reasoningEffort != "" {
			chat["reasoning_effort"] = reasoningEffort
		}
	}
	raw, err := json.Marshal(chat)
	return raw, originalTools, originalToolChoice, reasoningEffort, err
}

func newResponseObject(id string, created int64, req responseCreateRequest, tools []any, toolChoice any, effort string) map[string]any {
	store := req.Store == nil || *req.Store
	previous := any(nil)
	if req.PreviousResponseID != "" {
		previous = req.PreviousResponseID
	}
	instructions := any(nil)
	if req.Instructions != "" {
		instructions = req.Instructions
	}
	reasoning := map[string]any{"effort": effort, "summary": nil}
	if effort == "" {
		reasoning["effort"] = nil
	}
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": created, "status": "in_progress",
		"background": false, "error": nil, "incomplete_details": nil,
		"instructions": instructions, "max_output_tokens": req.MaxOutputTokens,
		"model": req.Model, "output": []any{}, "parallel_tool_calls": parallel,
		"previous_response_id": previous, "reasoning": reasoning, "store": store,
		"temperature": req.Temperature, "tool_choice": toolChoice, "tools": tools,
		"top_p": req.TopP, "truncation": "disabled", "usage": nil,
		"metadata": req.Metadata,
	}
}

func chatResponseToResponse(base, chat map[string]any) (map[string]any, []any, error) {
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, nil, fmt.Errorf("upstream response contains no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, nil, fmt.Errorf("upstream response contains no assistant message")
	}
	output, generated := chatMessageToResponseOutput(message)
	final := cloneMap(base)
	final["status"] = "completed"
	final["output"] = output
	final["usage"] = responseUsage(chat["usage"])
	reason, _ := choice["finish_reason"].(string)
	if reason == "length" || reason == "content_filter" || reason == "" {
		final["status"] = "incomplete"
		if reason == "length" {
			reason = "max_output_tokens"
		}
		if reason == "" {
			reason = "upstream_incomplete"
		}
		final["incomplete_details"] = map[string]any{"reason": reason}
	}
	return final, generated, nil
}

func chatMessageToResponseOutput(message map[string]any) ([]any, []any) {
	output := []any{}
	generated := []any{}
	if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
		output = append(output, responseReasoningItem(newResponseID("rs"), "completed", reasoning))
	}
	if content, ok := message["content"].(string); ok && content != "" {
		item := responseMessageItem(newResponseID("msg"), "completed", content)
		output = append(output, item)
		generated = append(generated, map[string]any{"role": "assistant", "content": content})
	}
	if calls := anySlice(message["tool_calls"]); len(calls) > 0 {
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			callID, _ := call["id"].(string)
			if callID == "" {
				callID = newResponseID("call")
			}
			name, _ := fn["name"].(string)
			arguments, _ := fn["arguments"].(string)
			output = append(output, responseFunctionItem(functionItemID(callID), callID, name, arguments, "completed"))
		}
		generated = append(generated, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls})
	}
	generated = preserveResponseReasoning(generated, message["reasoning_content"])
	return output, generated
}

// The summary channel carries only reasoning text actually exposed by the upstream.
// It is a compatibility mapping, not an independently generated summary.
func responseReasoningItem(id, status, text string) map[string]any {
	return map[string]any{"id": id, "type": "reasoning", "status": status,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}}}
}

func preserveResponseReasoning(generated []any, raw any) []any {
	text, _ := raw.(string)
	if text == "" {
		return generated
	}
	if len(generated) == 0 {
		generated = append(generated, map[string]any{"role": "assistant", "content": nil})
	}
	for _, rawMessage := range generated {
		rawMessage.(map[string]any)["reasoning_content"] = text
	}
	return generated
}

func anySlice(value any) []any {
	switch items := value.(type) {
	case []any:
		return items
	case []map[string]any:
		out := make([]any, len(items))
		for i := range items {
			out[i] = items[i]
		}
		return out
	default:
		return nil
	}
}

func responseMessageItem(id, status, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "status": status, "role": "assistant", "content": []any{
		map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text},
	}}
}

func responseFunctionItem(id, callID, name, arguments, status string) map[string]any {
	return map[string]any{"id": id, "type": "function_call", "status": status, "call_id": callID, "name": name, "arguments": arguments}
}

func functionItemID(callID string) string {
	suffix := strings.TrimPrefix(callID, "call_")
	if suffix == "" {
		suffix = newResponseID("")
	}
	return "fc_" + suffix
}

func responseUsage(raw any) any {
	u, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	input := numericValue(u["prompt_tokens"])
	output := numericValue(u["completion_tokens"])
	total := numericValue(u["total_tokens"])
	if total == nil && input != nil && output != nil {
		total = numericToFloat(input) + numericToFloat(output)
	}
	result := map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": total,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if cached := numericValue(details["cached_tokens"]); cached != nil {
			result["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := u["completion_tokens_details"].(map[string]any); ok {
		if reasoning := numericValue(details["reasoning_tokens"]); reasoning != nil {
			result["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	return result
}

func numericValue(v any) any {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return n
	case int64:
		return n
	default:
		return nil
	}
}

func numericToFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

func newResponseID(prefix string) string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	if prefix == "" {
		return hex.EncodeToString(data[:])
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}

func writeResponsesError(w http.ResponseWriter, status int, code, message string, param any) {
	// Do not log the error text: malformed JSON, custom field names and IDs can
	// include secrets. The request ID and fixed error code identify the failure.
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		requestID = newResponseID("req")
		w.Header().Set("X-Request-ID", requestID)
	}
	log.Printf("[responses] request_id=%s status=%d code=%s param=%v", requestID, status, code, param)
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error", "code": code, "param": param,
	}})
}

func copyHeader(dst, src http.Header) {
	for key := range dst {
		dst.Del(key)
	}
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type captureResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{header: make(http.Header), status: http.StatusOK}
}
func (w *captureResponseWriter) Header() http.Header         { return w.header }
func (w *captureResponseWriter) WriteHeader(status int)      { w.status = status }
func (w *captureResponseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
