package upstream

import (
	"encoding/json"
	"testing"
)

func TestPrepareBodyMirrorsAssistantReasoningHistory(t *testing.T) {
	body := `{"model":"kimi-k2.6","messages":[
		{"role":"user","content":"hello","reasoning_content":"user text"},
		{"role":"assistant","content":"","reasoning_content":"first thought","reasoning_details":[{"text":"ignored"}]},
		{"role":"assistant","content":"","reasoning_details":[{"text":"second "},{"summary":"thought"},null]},
		{"role":"assistant","content":"","reasoning":"explicit","reasoning_content":"other"},
		{"role":"assistant","content":"plain","reasoning_details":[{"text":42}]}
	]}`
	var out map[string]any
	if err := json.Unmarshal(PrepareBodyOpt([]byte(body), false), &out); err != nil {
		t.Fatal(err)
	}
	msgs := out["messages"].([]any)
	for _, tc := range []struct {
		index int
		want  string
	}{{1, "first thought"}, {2, "second thought"}, {3, "explicit"}} {
		msg := msgs[tc.index].(map[string]any)
		if got := msg["reasoning"]; got != tc.want {
			t.Errorf("message %d reasoning = %v, want %q", tc.index, got, tc.want)
		}
	}
	if _, ok := msgs[0].(map[string]any)["reasoning"]; ok {
		t.Error("user reasoning must not be changed")
	}
	if _, ok := msgs[4].(map[string]any)["reasoning"]; ok {
		t.Error("invalid details must not create reasoning")
	}
	if got := msgs[1].(map[string]any)["reasoning_content"]; got != "first thought" {
		t.Errorf("source reasoning_content lost: %v", got)
	}
}

func TestPrepareBodySanitizesAllReasoningRepresentations(t *testing.T) {
	body := `{"model":"kimi-k2.6","messages":[{"role":"assistant","reasoning_content":"11128","reasoning_details":[{"text":"11128","summary":"11128"}]},{"role":"assistant","reasoning":"11128"}]}`
	var out map[string]any
	if err := json.Unmarshal(PrepareBodyOpt([]byte(body), true), &out); err != nil {
		t.Fatal(err)
	}
	msgs := out["messages"].([]any)
	first := msgs[0].(map[string]any)
	detail := first["reasoning_details"].([]any)[0].(map[string]any)
	for name, value := range map[string]any{"reasoning": first["reasoning"], "reasoning_content": first["reasoning_content"], "detail text": detail["text"], "detail summary": detail["summary"], "explicit reasoning": msgs[1].(map[string]any)["reasoning"]} {
		if value != "11-128" {
			t.Errorf("%s = %v, want sanitized text", name, value)
		}
	}
}

func TestPrepareBodyDeepSeekReasoningDetailsCompose(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":"","reasoning_details":[{"text":"thought"}]}]}`
	var out map[string]any
	if err := json.Unmarshal(PrepareBodyOpt([]byte(body), false), &out); err != nil {
		t.Fatal(err)
	}
	msg := out["messages"].([]any)[0].(map[string]any)
	if msg["reasoning"] != "thought" || msg["reasoning_content"] != "thought" {
		t.Errorf("reasoning history not preserved: %v", msg)
	}
}
