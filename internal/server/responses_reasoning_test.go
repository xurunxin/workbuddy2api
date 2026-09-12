package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestResponsesReasoningEndToEnd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, ending := range []string{"stop", "tool_calls", "length", ""} {
			t.Run(fmt.Sprintf("stream=%t/ending=%s", stream, ending), func(t *testing.T) {
				raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先思考\"}}]}\n\n" +
					"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"再回答\"}}]}\n\n"
				if ending == "stop" {
					raw += "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"答案\"}}]}\n\n"
				} else if ending == "tool_calls" {
					raw += "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_test\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n"
				}
				raw += fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", ending)
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"deepseek-v4.1-flash","input":"test","stream":%t,"reasoning":{"effort":"high","summary":"auto"}}`, stream))))
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
				var final map[string]any
				if stream {
					assertIncreasingSequences(t, rec.Body.String())
					for _, event := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done"} {
						if !strings.Contains(rec.Body.String(), "event: "+event+"\n") {
							t.Errorf("missing %s", event)
						}
					}
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						if event["type"] == "response.completed" || event["type"] == "response.incomplete" {
							final = event["response"].(map[string]any)
						}
					}
				} else if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
					t.Fatal(err)
				}
				if final == nil {
					t.Fatal("missing terminal response")
				}
				output := final["output"].([]any)
				reasoning := output[0].(map[string]any)
				if reasoning["type"] != "reasoning" || reasoning["summary"].([]any)[0].(map[string]any)["text"] != "先思考再回答" {
					t.Fatalf("output=%v", output)
				}
				if ending == "stop" || ending == "tool_calls" {
					if len(output) != 2 || final["status"] != "completed" {
						t.Fatalf("final=%v", final)
					}
				} else if final["status"] != "incomplete" {
					t.Fatalf("final=%v", final)
				}
				stored, ok := h.responses.get(final["id"].(string))
				if !ok {
					t.Fatal("missing stored response")
				}
				last := stored.history[len(stored.history)-1].(map[string]any)
				if last["reasoning_content"] != "先思考再回答" {
					t.Fatalf("history=%v", stored.history)
				}
				{
					input, _ := json.Marshal(output)
					messages, err := responseInputToMessages(input)
					if err != nil {
						t.Fatal(err)
					}
					if messages[0].(map[string]any)["reasoning_content"] != "先思考再回答" {
						t.Fatalf("replay=%v", messages)
					}
				}
			})
		}
	}
}

func TestResponsesReasoningImmediateAndFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, map[string]any{"output": []any{}}, nil)
	frame := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial\"}}]}\n\n")
	// Exercise fragmented writes, including within the field name.
	for _, b := range frame {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(rec.Body.String(), "event: response.reasoning_summary_text.delta\n") {
		t.Fatal("reasoning was buffered until completion")
	}
	if strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatal("premature completion")
	}
	w.fail("upstream_error", "failed")
	before := rec.Body.String()
	_, _ = w.Write(frame)
	if rec.Body.String() != before {
		t.Fatal("emitted reasoning after terminal event")
	}
	if !strings.Contains(before, `"text":"partial"`) || !strings.Contains(before, "response.failed") {
		t.Fatal("lost partial reasoning")
	}
}
