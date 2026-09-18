package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestResponsesFunctionOutputContent(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		invalid            bool
	}{
		{"string", `"result"`, "result", false},
		{"text list", `[{"type":"input_text","text":"first"},{"type":"input_text","text":"第二行"}]`, "first\n第二行", false},
		{"empty list", `[]`, "", false},
		{"empty string", `""`, "", false},
		{"null", `null`, "", true},
		{"object", `{}`, "", true},
		{"number", `42`, "", true},
		{"boolean", `true`, "", true},
		{"null part", `[null]`, "", true},
		{"missing text", `[{"type":"input_text"}]`, "", true},
		{"null text", `[{"type":"input_text","text":null}]`, "", true},
		{"image without URL", `[{"type":"input_image"}]`, "", true},
		{"file", `[{"type":"input_file","file_id":"file_1"}]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reproduce the reported index in a long conversation.
			prefix := strings.Repeat(`{"role":"user","content":"history"},`, 400)
			input := `[` + prefix + `{"type":"function_call_output","call_id":"call_1","output":` + tc.output + `}]`
			messages, err := responseInputToMessages(json.RawMessage(input))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "input[400] function_call_output.output") {
					t.Fatalf("expected indexed validation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			message := messages[400].(map[string]any)
			if message["role"] != "tool" || message["tool_call_id"] != "call_1" || message["content"] != tc.want {
				t.Fatalf("tool message = %v", message)
			}
		})
	}
}

func TestResponsesOptionalPreferencesAcceptedAndNotForwarded(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{
			name:  "encrypted reasoning include",
			field: `"include":["reasoning.encrypted_content"]`,
		},
		{
			name:  "reasoning effort and summary",
			field: `"reasoning":{"effort":"low","summary":"auto"}`,
		},
		{
			name:  "text format and verbosity",
			field: `"text":{"format":{"type":"text"},"verbosity":"medium"}`,
		},
		{
			name:  "stream obfuscation option",
			field: `"stream_options":{"include_obfuscation":false}`,
		},
		{
			name:  "prompt cache key",
			field: `"prompt_cache_key":"session-pi-0"`,
		},
		{
			name:  "prompt cache key at maximum length",
			field: fmt.Sprintf(`"prompt_cache_key":"%s"`, strings.Repeat("k", 64)),
		},
		{
			name:  "prompt cache retention in memory",
			field: `"prompt_cache_retention":"in-memory"`,
		},
		{
			name:  "prompt cache retention twenty four hours",
			field: `"prompt_cache_retention":"24h"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var outbound map[string]any
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseOK, true
			})
			// Capture the request after constructing the fake so this test can
			// verify the Responses-only options are not sent to Chat upstream.
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
					t.Fatalf("decode upstream request: %v", err)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sseOK)),
				}, nil
			})
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
			})

			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"glm-5.2","input":"hello",%s}`, tc.field)
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "encrypted_content") {
				t.Errorf("gateway fabricated encrypted reasoning content: %s", rec.Body)
			}
			if outbound == nil {
				t.Fatal("upstream was not called")
			}
			for _, key := range []string{"include", "reasoning", "text", "prompt_cache_retention"} {
				if _, ok := outbound[key]; ok {
					t.Errorf("Responses option %q was forwarded upstream: %v", key, outbound)
				}
			}
			if tc.name == "reasoning effort and summary" && outbound["reasoning_effort"] != "low" {
				t.Errorf("reasoning effort was not mapped: %v", outbound)
			}
		})
	}
}

func TestResponsesOptionalPreferencesCombinedStreamStoreFalse(t *testing.T) {
	var outbound map[string]any
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	body := `{"model":"glm-5.2","input":"hello","include":["reasoning.encrypted_content"],"reasoning":{"effort":"low","summary":"auto"},"text":{"format":{"type":"text"},"verbosity":"medium"},"stream_options":{"include_obfuscation":false},"prompt_cache_key":"session-pi-0","prompt_cache_retention":"24h","stream":true,"store":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "event: response.completed\n") {
		t.Fatalf("stream did not complete: %s", rec.Body)
	}
	for _, key := range []string{"include", "reasoning", "text", "prompt_cache_retention"} {
		if _, ok := outbound[key]; ok {
			t.Errorf("Responses option %q was forwarded upstream: %v", key, outbound)
		}
	}
	if outbound["stream"] != true {
		t.Errorf("stream was not forwarded as a Chat request option: %v", outbound)
	}
	if _, ok := outbound["store"]; ok {
		t.Errorf("Responses store option was forwarded upstream: %v", outbound)
	}

	var completed map[string]any
	if err := json.Unmarshal([]byte(lastResponseJSONData(rec.Body.String())), &completed); err != nil {
		t.Fatalf("decode final event: %v", err)
	}
	response, ok := completed["response"].(map[string]any)
	if !ok {
		t.Fatalf("final event has no response: %v", completed)
	}
	if response["store"] != false {
		t.Fatalf("store=false was not reflected in response: %v", response)
	}
	id, _ := response["id"].(string)
	if id == "" {
		t.Fatalf("stream response did not include id: %v", response)
	}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if get.Code != http.StatusNotFound {
		t.Fatalf("store=false response was retained: code=%d body=%s", get.Code, get.Body)
	}
}

func TestResponsesHarnessMaxReasoning(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var outbound map[string]any
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			// Redacted regression fixture matching the exported Harness session's
			// model/effort and the SDK's actual Responses message envelope.
			body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":[{"type":"input_text","text":"Reply with OK only."}]}],"stream":%t,"store":false,"reasoning":{"effort":"max","summary":"auto"},"include":["reasoning.encrypted_content"],"prompt_cache_key":"regression-harness-max"}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if outbound["reasoning_effort"] != "max" {
				t.Fatalf("max was lost before upstream execution: %v", outbound["reasoning_effort"])
			}
			if stream && !strings.Contains(rec.Body.String(), "event: response.completed\n") {
				t.Fatalf("stream did not complete: %s", rec.Body)
			}
			if !stream {
				var response map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response["status"] != "completed" || response["reasoning"].(map[string]any)["effort"] != "max" {
					t.Fatalf("unexpected response state: %v", response)
				}
			}
		})
	}
}

func TestResponsesEmptyIncludeArrayAccepted(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hello","include":[]}`)))
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("code=%d calls=%d body=%s", rec.Code, calls, rec.Body)
	}
}

func TestResponsesAssistantInputTextHistoryAccepted(t *testing.T) {
	var outbound map[string]any
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":[{"role":"assistant","content":[{"type":"input_text","text":"previous assistant"}]},{"role":"user","content":[{"type":"input_text","text":"continue"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	msgs, ok := outbound["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages=%v", outbound["messages"])
	}
	assistant, _ := msgs[0].(map[string]any)
	if assistant["role"] != "assistant" || !strings.Contains(fmt.Sprint(assistant["content"]), "previous assistant") {
		t.Fatalf("assistant input_text was not converted: %v", assistant)
	}
}

func TestResponsesHarnessAssistantMessageHistoryAccepted(t *testing.T) {
	var outbound map[string]any
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":[{"type":"message","role":"assistant","id":"msg_pi_0","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	msgs, ok := outbound["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages=%v", outbound["messages"])
	}
	assistant, _ := msgs[0].(map[string]any)
	if assistant["role"] != "assistant" || !strings.Contains(fmt.Sprint(assistant["content"]), "OK") {
		t.Fatalf("Harness assistant message was not converted: %v", assistant)
	}
}

func TestResponsesRejectInvalidOptionalPreferencesBeforeUpstream(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	invalidBodies := []string{
		`{"model":"glm-5.2","input":"hello","include":"reasoning.encrypted_content"}`,
		`{"model":"glm-5.2","input":"hello","include":["unknown.include"]}`,
		`{"model":"glm-5.2","input":"hello","reasoning":{"effort":true}}`,
		`{"model":"glm-5.2","input":"hello","reasoning":{"effort":"unsupported"}}`,
		`{"model":"glm-5.2","input":"hello","reasoning":{"summary":"unsupported"}}`,
		`{"model":"glm-5.2","input":"hello","text":{"format":{"type":"json_schema"}}}`,
		`{"model":"glm-5.2","input":"hello","stream_options":{"include_obfuscation":"false"}}`,
		`{"model":"glm-5.2","input":"hello","background":true}`,
		`{"model":"glm-5.2","input":"hello","prompt_cache_key":123}`,
		`{"model":"glm-5.2","input":"hello","prompt_cache_retention":true}`,
		`{"model":"glm-5.2","input":"hello","prompt_cache_retention":"7d"}`,
	}
	invalidBodies = append(invalidBodies, fmt.Sprintf(
		`{"model":"glm-5.2","input":"hello","prompt_cache_key":"%s"}`,
		strings.Repeat("k", 65),
	))
	for _, body := range invalidBodies {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d response=%s", body, rec.Code, rec.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached upstream %d times", calls)
	}
}

func lastResponseJSONData(body string) string {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "data: ") {
			return strings.TrimPrefix(lines[i], "data: ")
		}
	}
	return ""
}
