package server

import (
	"context"
	"log"
	"net/http"
)

type usageContextKey struct{}
type requestUsage struct {
	input, output int64
	known         bool
	failed        bool
}

type usageWriter struct {
	http.ResponseWriter
	status int
}

func (w *usageWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *usageWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *usageWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *usageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *Handler) trackUsage(w http.ResponseWriter, r *http.Request, key string, next http.HandlerFunc) {
	if h.cfg.Usage == nil || r.Method != "POST" || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/responses") {
		next(w, r)
		return
	}
	u := &requestUsage{}
	wrapper := &usageWriter{ResponseWriter: w}
	defer func() {
		status := wrapper.status
		if status == 0 {
			status = 500
		}
		if u.failed {
			status = 502
		}
		if r.Context().Err() != nil {
			status = 499
		}
		if err := h.cfg.Usage.Record(key, status, u.input, u.output, u.known); err != nil {
			log.Printf("ERR: [usage] persist statistics failed")
		}
	}()
	next(wrapper, r.WithContext(context.WithValue(r.Context(), usageContextKey{}, u)))
}
