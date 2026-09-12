package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesValidationErrorDiagnosticsDoNotLogClientValues(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, body := range []string{
		`{"text":{"format":{"type":"private-client-value"}}}`,
		`{"private-client-value":true}`,
	} {
		logs.Reset()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			t.Fatal(err)
		}
		err := validateResponseFields(fields)
		if err == nil {
			t.Fatal("expected invalid preference to fail")
		}
		w := httptest.NewRecorder()
		writeResponsesError(w, http.StatusBadRequest, "unsupported_parameter", err.Error(), responseErrorParam(err))
		requestID := w.Header().Get("X-Request-ID")
		if requestID == "" || !strings.Contains(logs.String(), requestID) || !strings.Contains(logs.String(), "status=400") {
			t.Fatalf("missing correlatable error diagnostics: %s", logs.String())
		}
		if strings.Contains(logs.String(), "private-client-value") {
			t.Fatal("client data leaked into error log")
		}
		if _, known := fields["text"]; known && !strings.Contains(logs.String(), "param=text") {
			t.Fatalf("known failed parameter not recorded: %s", logs.String())
		}
	}
}
