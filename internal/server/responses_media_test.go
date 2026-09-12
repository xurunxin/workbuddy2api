package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestResponsesMultimodalForwardingAndHistory(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"image URL", `{"type":"input_image","image_url":"https://example.test/image.png","detail":"high"}`, `{"type":"image_url","image_url":{"url":"https://example.test/image.png","detail":"high"}}`},
		{"image data", `{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}`, `{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}`},
		{"file", `{"type":"input_file","filename":"report.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}`, `{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,aGVsbG8="}}`},
		{"audio file", `{"type":"input_file","filename":"clip.wav","file_data":"data:audio/wav;base64,aGVsbG8="}`, `{"type":"file","file":{"filename":"clip.wav","file_data":"data:audio/wav;base64,aGVsbG8="}}`},
		{"video file", `{"type":"input_file","filename":"clip.mp4","file_data":"data:video/mp4;base64,aGVsbG8="}`, `{"type":"file","file":{"filename":"clip.mp4","file_data":"data:video/mp4;base64,aGVsbG8="}}`},
		{"raw file base64", `{"type":"input_file","filename":"clip.mp3","file_data":"aGVsbG8="}`, `{"type":"file","file":{"filename":"clip.mp3","file_data":"aGVsbG8="}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var outbound map[string]any
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
				up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
				})
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, PromptMode: "passthrough"})
				// Two parallel results must both precede the synthetic user message.
				body := fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"input":[
					{"role":"user","content":[%s]},
					{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
					{"type":"function_call","call_id":"call_b","name":"read","arguments":"{}"},
					{"type":"function_call_output","call_id":"call_a","output":[{"type":"input_text","text":"before"},%s,{"type":"input_text","text":"after"}]},
					{"type":"function_call_output","call_id":"call_b","output":[%s]}]}`, stream, tc.input, tc.input, tc.input)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("%d: %s", rec.Code, rec.Body)
				}
				messages := outbound["messages"].([]any)
				if len(messages) != 5 {
					t.Fatalf("messages=%v", messages)
				}
				var want any
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Fatal(err)
				}
				if got := messages[0].(map[string]any)["content"].([]any)[0]; !reflect.DeepEqual(got, want) {
					t.Fatalf("user media=%v, want=%v", got, want)
				}
				for i, id := range []string{"call_a", "call_b"} {
					tool := messages[i+2].(map[string]any)
					if tool["role"] != "tool" || tool["tool_call_id"] != id {
						t.Fatalf("tool=%v", tool)
					}
					if _, ok := tool["content"].(string); !ok {
						t.Fatal("tool content must remain textual")
					}
				}
				mediaMessage := messages[4].(map[string]any)
				parts := mediaMessage["content"].([]any)
				if mediaMessage["role"] != "user" || len(parts) != 6 {
					t.Fatalf("media=%v", mediaMessage)
				}
				if !strings.Contains(parts[0].(map[string]any)["text"].(string), "call_a") || !strings.Contains(parts[4].(map[string]any)["text"].(string), "call_b") || parts[1].(map[string]any)["text"] != "before" || parts[3].(map[string]any)["text"] != "after" || !reflect.DeepEqual(parts[2], want) || !reflect.DeepEqual(parts[5], want) {
					t.Fatalf("media order/data=%v", parts)
				}
				// Read the stored response ID from either transport and resume it.
				var response map[string]any
				if !stream {
					_ = json.Unmarshal(rec.Body.Bytes(), &response)
				} else {
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.completed" {
							response, _ = event["response"].(map[string]any)
						}
					}
				}
				if response["id"] == nil {
					t.Fatalf("missing completed response: %s", rec.Body)
				}
				followup := fmt.Sprintf(`{"model":"glm-5.2","previous_response_id":%q,"input":"continue"}`, response["id"])
				rec = httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(followup)))
				if rec.Code != 200 {
					t.Fatalf("resume: %d %s", rec.Code, rec.Body)
				}
				continued := outbound["messages"].([]any)
				if len(continued) != 7 || !reflect.DeepEqual(continued[:5], messages) {
					t.Fatalf("history changed: %v", continued)
				}
			})
		}
	}
}

func TestResponsesMultimodalInvalidInputs(t *testing.T) {
	for _, part := range []string{
		`{"type":"input_image","file_id":"file_a"}`,
		`{"type":"input_image","image_url":"C:/private/image.png"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,???"}`,
		`{"type":"input_file","file_url":"https://example.test/a.pdf"}`,
		`{"type":"input_file","filename":"a.pdf","file_data":null}`,
		`{"type":"input_audio","input_audio":null}`,
		`{"type":"input_audio","input_audio":{"data":"aGVsbG8=","format":"wav"}}`,
		`{"type":"input_audio","input_audio":{"data":"???","format":"wav"}}`,
		`{"type":"input_audio","input_audio":{"data":"data:video/mp4;base64,aGVsbG8=","format":"wav"}}`,
		`{"type":"input_audio","input_audio":{"data":"aGVsbG8=","format":"bad"}}`,
		`{"type":"input_video","video_url":null}`,
		`{"type":"input_video","video_url":"https://example.test/a.mp4"}`,
		`{"type":"video_url","video_url":{"url":"https://example.test/a.mp4"}}`,
		`{"type":"input_video","video_url":"file:///private/a.mp4"}`,
		`{"type":"video_url","video_url":{"url":123}}`,
		`{"type":"input_video","video_url":"https://example.test/a.mp4","fps":null}`,
		`{"type":"input_video","video_url":"https://example.test/a.mp4","fps":-1}`,
	} {
		for _, input := range []string{
			`[{"role":"user","content":[` + part + `]}]`,
			`[{"type":"function_call_output","call_id":"call_a","output":[` + part + `]}]`,
		} {
			if _, err := responseInputToMessages(json.RawMessage(input)); err == nil || !strings.Contains(err.Error(), "input[0]") {
				t.Fatalf("expected indexed error for %s: %v", input, err)
			}
		}
	}
}

func TestResponsesImageOutputAtReportedIndex(t *testing.T) {
	input := `[` + strings.Repeat(`{"role":"user","content":"history"},`, 399) +
		`{"type":"function_call","call_id":"read_image","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"read_image","output":[{"type":"input_image","image_url":"https://example.test/a.png"}]},` +
		`{"role":"assistant","content":"I can see the image"}]`
	messages, err := responseInputToMessages(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 403 || messages[400].(map[string]any)["role"] != "tool" || messages[401].(map[string]any)["role"] != "user" || messages[402].(map[string]any)["role"] != "assistant" {
		t.Fatalf("image output did not flush before assistant: %v", messages[399:])
	}
}

func TestResponsesMediaRoleRestrictions(t *testing.T) {
	for _, role := range []string{"assistant", "system", "developer"} {
		for _, part := range []string{
			`{"type":"input_image","image_url":"https://example.test/a.png"}`,
			`{"type":"input_file","filename":"a.pdf","file_data":"aGVsbG8="}`,
			`{"type":"input_audio","input_audio":{"data":"aGVsbG8=","format":"wav"}}`,
			`{"type":"input_video","video_url":"https://example.test/a.mp4"}`,
		} {
			input := fmt.Sprintf(`[{"role":%q,"content":[%s]}]`, role, part)
			if _, err := responseInputToMessages(json.RawMessage(input)); err == nil {
				t.Fatalf("expected unsupported media role: %s", input)
			}
		}
	}
}
