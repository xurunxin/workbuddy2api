package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestResponsesNonStreamMapsRequestAndResponse(t *testing.T) {
	var outbound map[string]any
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &outbound); err != nil {
				t.Fatalf("outbound: %v", err)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, APIKey: "secret", PromptMode: "passthrough"})
	body := `{
		"model":"glm-5.2","instructions":"Be concise","input":[
			{"role":"user","content":[{"type":"input_text","text":"weather"},{"type":"input_image","image_url":"https://example.test/a.png","detail":"low"}]}
		],
		"tools":[{"type":"function","name":"weather","description":"look up","parameters":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"function","name":"weather"},"reasoning":{"effort":"high"},"max_output_tokens":123
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if outbound["max_tokens"] != float64(123) || outbound["reasoning_effort"] != "high" || outbound["stream"] != true {
		t.Fatalf("mapped fields=%v", outbound)
	}
	msgs := outbound["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "Be concise" {
		t.Fatalf("instructions not mapped: %v", msgs)
	}
	tools := outbound["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "weather" || fn["strict"] != true {
		t.Fatalf("tool=%v", tools[0])
	}
	if outbound["tool_choice"] != "weather" {
		t.Fatalf("normalized tool_choice=%v", outbound["tool_choice"])
	}

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("response=%v", response)
	}
	output := response["output"].([]any)
	message := output[0].(map[string]any)
	part := message["content"].([]any)[0].(map[string]any)
	if message["type"] != "message" || part["type"] != "output_text" || part["text"] != "你好" {
		t.Fatalf("output=%v", output)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestResponsesStreamingSemanticEventsAndToolFragments(t *testing.T) {
	raw := "data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi \"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"there\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, event := range []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.output_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.output_item.done", "response.completed",
	} {
		if !strings.Contains(body, "event: "+event+"\n") {
			t.Errorf("missing %s in %s", event, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("Responses stream must end with semantic terminal event: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"city\":\"Paris\"}"`) {
		t.Errorf("fragmented arguments not assembled: %s", body)
	}
	assertIncreasingSequences(t, body)
}

func TestResponsesStreamWriterEmitsDeltaBeforeTerminalFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	base := map[string]any{"id": "resp_test", "object": "response", "status": "in_progress", "output": []any{}}
	w := newResponsesStreamWriter(rec, base, nil)
	frame := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
	if _, err := w.Write(frame); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta\n") || !strings.Contains(body, `"delta":"first"`) {
		t.Fatalf("delta was not emitted immediately: %s", body)
	}
	if strings.Contains(body, "response.completed") || strings.Contains(body, "response.incomplete") {
		t.Fatalf("terminal event emitted before upstream terminal frame: %s", body)
	}
}

func TestResponsesStreamBuffersToolArgumentsUntilIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	base := map[string]any{"id": "resp_test", "object": "response", "status": "in_progress", "output": []any{}}
	w := newResponsesStreamWriter(rec, base, nil)
	first := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n")
	if _, err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "response.function_call_arguments.delta") || strings.Contains(rec.Body.String(), `"type":"function_call"`) {
		t.Fatalf("function event emitted without stable identity: %s", rec.Body)
	}
	second := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_late\",\"function\":{\"name\":\"lookup\",\"arguments\":\"1}\"}}]}}]}\n\n")
	if _, err := w.Write(second); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	added := strings.Index(body, "event: response.output_item.added")
	delta := strings.Index(body, "event: response.function_call_arguments.delta")
	if added < 0 || delta < 0 || added > delta {
		t.Fatalf("identity must precede buffered delta: %s", body)
	}
	if !strings.Contains(body, `"call_id":"call_late"`) || !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `"delta":"{\"x\":1}"`) {
		t.Fatalf("buffered function fields missing: %s", body)
	}
}

func TestResponsesNonStreamWithoutFinishReasonIsIncomplete(t *testing.T) {
	raw := "data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response["status"] != "incomplete" {
		t.Fatalf("status=%v response=%v", response["status"], response)
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details["reason"] != "upstream_incomplete" {
		t.Fatalf("details=%v", details)
	}
}

func TestResponsesPreviousResponseContinuesParallelFunctionCalls(t *testing.T) {
	toolSSE := "data: {\"id\":\"x\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"a\",\"arguments\":\"{}\"}}," +
		"{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"b\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	var mu sync.Mutex
	var bodies []map[string]any
	calls := 0
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		calls++
		call := calls
		mu.Unlock()
		stream := sseOK
		if call == 1 {
			stream = toolSSE
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}, ChatBaseCN: "https://fake.example"}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, PromptMode: "passthrough"})
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"run both","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}]}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	var firstResponse map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &firstResponse)
	if got := len(firstResponse["output"].([]any)); got != 2 {
		t.Fatalf("parallel output items=%d response=%v", got, firstResponse)
	}
	id := firstResponse["id"].(string)
	secondInput := `{"model":"glm-5.2","previous_response_id":"` + id + `","input":[` +
		`{"type":"function_call_output","call_id":"call_a","output":"A"},` +
		`{"type":"function_call_output","call_id":"call_b","output":"B"}]}`
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondInput)))
	if second.Code != http.StatusOK {
		t.Fatalf("second=%d %s", second.Code, second.Body)
	}
	mu.Lock()
	continued := bodies[1]
	mu.Unlock()
	messages := continued["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages=%v", messages)
	}
	assistant := messages[1].(map[string]any)
	if got := len(assistant["tool_calls"].([]any)); got != 2 {
		t.Fatalf("assistant calls=%v", assistant)
	}
	if messages[2].(map[string]any)["tool_call_id"] != "call_a" || messages[3].(map[string]any)["tool_call_id"] != "call_b" {
		t.Fatalf("tool outputs=%v", messages[2:])
	}
}

type contextBlockingBody struct {
	ctx       context.Context
	read      chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func (b *contextBlockingBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.read) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *contextBlockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestResponsesCancellationClosesUpstreamAndReleasesLease(t *testing.T) {
	started := make(chan *contextBlockingBody, 1)
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := &contextBlockingBody{ctx: r.Context(), read: make(chan struct{}), closed: make(chan struct{})}
		started <- body
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	body := <-started
	select {
	case <-body.read:
	case <-time.After(time.Second):
		t.Fatal("upstream body was not read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after cancellation")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body was not closed")
	}
	status, _ := p.Status("u1")
	if status.InFlight != 0 {
		t.Fatalf("in-flight lease leaked: %+v", status)
	}
}

func assertIncreasingSequences(t *testing.T, body string) {
	t.Helper()
	want := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("event json: %v", err)
		}
		if got := int(event["sequence_number"].(float64)); got != want {
			t.Fatalf("sequence=%d want=%d event=%v", got, want, event)
		}
		want++
	}
}

func TestResponsesStorePreviousGetDeleteAndStoreFalse(t *testing.T) {
	var outboundBodies []map[string]any
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			outboundBodies = append(outboundBodies, body)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})}, ChatBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, PromptMode: "passthrough"})
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"first"}`)))
	var firstResponse map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &firstResponse)
	id := firstResponse["id"].(string)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), id) {
		t.Fatalf("get code=%d body=%s", get.Code, get.Body)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"second","previous_response_id":"`+id+`"}`)))
	if second.Code != http.StatusOK {
		t.Fatalf("continuation=%d %s", second.Code, second.Body)
	}
	msgs := outboundBodies[1]["messages"].([]any)
	if len(msgs) != 3 || msgs[0].(map[string]any)["content"] != "first" || msgs[1].(map[string]any)["content"] != "你好" || msgs[2].(map[string]any)["content"] != "second" {
		t.Fatalf("continuation history=%v", msgs)
	}

	deleted := httptest.NewRecorder()
	h.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/v1/responses/"+id, nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body)
	}
	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("get deleted=%d", missing.Code)
	}

	notStored := httptest.NewRecorder()
	h.ServeHTTP(notStored, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"private","store":false}`)))
	var ns map[string]any
	_ = json.Unmarshal(notStored.Body.Bytes(), &ns)
	nsGet := httptest.NewRecorder()
	h.ServeHTTP(nsGet, httptest.NewRequest(http.MethodGet, "/v1/responses/"+ns["id"].(string), nil))
	if nsGet.Code != http.StatusNotFound {
		t.Fatalf("store:false retrievable: %d %s", nsGet.Code, nsGet.Body)
	}
}

func TestResponsesRejectUnsupportedCapabilitiesBeforeUpstream(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	for _, body := range []string{
		`{"model":"glm-5.2","input":"hi","background":true}`,
		`{"model":"glm-5.2","input":"hi","tools":[{"type":"web_search_preview"}]}`,
		`{"model":"glm-5.2","input":"hi","text":{"format":{"type":"json_schema"}}}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported") {
			t.Errorf("body=%s code=%d response=%s", body, rec.Code, rec.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("unsupported requests reached upstream %d times", calls)
	}
}

func TestResponsesRequiresSameAPIKeyAndBodyLimit(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, APIKey: "secret", MaxBodyBytes: 64})
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"x","input":"hi"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("auth code=%d", unauthorized.Code)
	}
	over := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", 65)))
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(over, req)
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("limit code=%d body=%s", over.Code, over.Body)
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached upstream")
	}
}
