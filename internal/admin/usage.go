package admin

import (
	"net/http"
	"time"
)

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Usage == nil {
		fail(w, 503, "统计存储未启用")
		return
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for _, value := range []string{from, to} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				fail(w, 400, "日期格式应为 YYYY-MM-DD")
				return
			}
		}
	}
	if from != "" && to != "" && from > to {
		fail(w, 400, "开始日期不能晚于结束日期")
		return
	}
	rows, degraded := h.cfg.Usage.Snapshot(from, to)
	respond(w, 200, map[string]any{"rows": rows, "credits": h.cfg.Usage.Credits(), "persistence_error": degraded, "timezone": "UTC"})
}
