// Package admin provides the authenticated, same-origin management console.
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accesskey"
	"workbuddy2api/internal/catalog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

type Config struct {
	Usage        *usage.Store
	Password     string
	SecureCookie bool
	AuthDir      string
	Pool         *pool.Pool
	Upstream     *upstream.Client
	Catalog      *catalog.Service
	KeyStore     *accesskey.Store
	ReadConfig   func() any
	SaveConfig   func(json.RawMessage) error
	Restart      func()
}

type session struct {
	csrf    string
	expires time.Time
}
type attempt struct {
	count   int
	expires time.Time
}
type Handler struct {
	cfg            Config
	mux            *http.ServeMux
	mu             sync.Mutex
	sessions       map[string]session
	attempts       map[string]attempt
	logins         map[string]*loginFlow
	busy           map[string]bool
	creditsChecked map[string]time.Time
	started        time.Time
}

const cookieName = "wb2a_admin"

func New(cfg Config) *Handler {
	if cfg.Catalog == nil {
		cfg.Catalog = catalog.New(cfg.Upstream)
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sessions: make(map[string]session), attempts: make(map[string]attempt), logins: make(map[string]*loginFlow), busy: make(map[string]bool), creditsChecked: make(map[string]time.Time), started: time.Now()}
	h.mux.HandleFunc("GET /admin/", h.assets)
	h.mux.HandleFunc("POST /admin/api/login", h.login)
	h.mux.HandleFunc("GET /admin/api/session", h.authorize(h.currentSession))
	h.mux.HandleFunc("POST /admin/api/logout", h.authorize(h.logout))
	h.mux.HandleFunc("GET /admin/api/status", h.authorize(h.status))
	h.mux.HandleFunc("GET /admin/api/config", h.authorize(h.readConfig))
	h.mux.HandleFunc("PUT /admin/api/config", h.authorize(h.saveConfig))
	h.mux.HandleFunc("POST /admin/api/restart", h.authorize(h.restart))
	h.mux.HandleFunc("POST /admin/api/accounts/import", h.authorize(h.importAccount))
	h.mux.HandleFunc("GET /admin/api/accounts/{uid}/models", h.authorize(h.accountModels))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/models/refresh", h.authorize(h.accountModels))
	h.mux.HandleFunc("GET /admin/api/keys", h.authorize(h.listKeys))
	h.mux.HandleFunc("GET /admin/api/usage", h.authorize(h.usage))
	h.mux.HandleFunc("POST /admin/api/keys", h.authorize(h.createKey))
	h.mux.HandleFunc("POST /admin/api/keys/{id}/revoke", h.authorize(h.revokeKey))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/{action}", h.authorize(h.accountAction))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.authorize(h.startOAuth))
	h.mux.HandleFunc("GET /admin/api/oauth/current", h.authorize(h.currentOAuth))
	h.mux.HandleFunc("POST /admin/api/oauth/{id}/poll", h.authorize(h.pollOAuth))
	h.mux.HandleFunc("DELETE /admin/api/oauth/{id}", h.authorize(h.cancelOAuth))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/") && r.Method != http.MethodGet {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				fail(w, 403, "跨站请求已拒绝")
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, 403, "跨站请求已拒绝")
			return
		}
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "application/json" {
			fail(w, 415, "请求必须使用 application/json")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"message": message}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "无效的 JSON 或请求超过 1 MB")
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "请求必须包含单个 JSON 对象")
		return false
	}
	return true
}

func (h *Handler) cleanupLocked() {
	now := time.Now()
	for k, s := range h.sessions {
		if !s.expires.After(now) {
			delete(h.sessions, k)
		}
	}
	for k, a := range h.attempts {
		if !a.expires.After(now) {
			delete(h.attempts, k)
		}
	}
	for k, f := range h.logins {
		s, sessionExists := h.sessions[f.owner]
		if !f.expires.After(now) || !sessionExists || !s.expires.After(now) {
			delete(h.logins, k)
		}
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Password == "" {
		fail(w, 503, "管理功能未启用：请设置至少 12 位的 WB2A_ADMIN_PASSWORD 后重启")
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	h.mu.Lock()
	h.cleanupLocked()
	a := h.attempts[ip]
	if a.count >= 8 || len(h.attempts) >= 2048 || len(h.sessions) >= 128 {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "300")
		fail(w, 429, "尝试次数过多，请 5 分钟后重试")
		return
	}
	if a.count == 0 {
		a.expires = time.Now().Add(5 * time.Minute)
	}
	a.count++
	h.attempts[ip] = a
	h.mu.Unlock()
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !equal(body.Password, h.cfg.Password) {
		fail(w, 401, "管理密码错误")
		return
	}
	id := randomID()
	s := session{randomID(), time.Now().Add(12 * time.Hour)}
	h.mu.Lock()
	delete(h.attempts, ip)
	h.sessions[id] = s
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/admin", HttpOnly: true, Secure: h.cfg.SecureCookie || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	respond(w, 200, map[string]any{"csrf_token": s.csrf, "expires_at": s.expires})
}

func (h *Handler) sessionFor(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", session{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[c.Value]
	return c.Value, s, ok && s.expires.After(time.Now())
}

func (h *Handler) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s, ok := h.sessionFor(r)
		if h.cfg.Password == "" || !ok {
			fail(w, 401, "请先登录管理控制台")
			return
		}
		if r.Method != "GET" && !equal(r.Header.Get("X-CSRF-Token"), s.csrf) {
			fail(w, 403, "会话校验失败，请刷新页面重试")
			return
		}
		next(w, r)
	}
}
func (h *Handler) currentSession(w http.ResponseWriter, r *http.Request) {
	_, s, _ := h.sessionFor(r)
	respond(w, 200, map[string]any{"csrf_token": s.csrf, "expires_at": s.expires})
}
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	id, _, _ := h.sessionFor(r)
	h.mu.Lock()
	delete(h.sessions, id)
	for k, f := range h.logins {
		if f.owner == id {
			delete(h.logins, k)
		}
	}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: h.cfg.SecureCookie || r.TLS != nil, SameSite: http.SameSiteStrictMode})
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, full := h.cfg.Pool.CountsDetailed()
	h.mu.Lock()
	checked := make(map[string]time.Time, len(h.creditsChecked))
	for k, v := range h.creditsChecked {
		checked[k] = v
	}
	h.mu.Unlock()
	respond(w, 200, map[string]any{"service": "workbuddy2api", "total": total, "healthy": healthy, "cooling": cooling, "disabled": disabled, "in_flight_full": full, "ready": h.cfg.Pool.ServableNow(), "accounts": h.cfg.Pool.List(), "credits_checked_at": checked, "started_at": h.started, "uptime_seconds": int64(time.Since(h.started).Seconds())})
}
func (h *Handler) readConfig(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadConfig == nil {
		fail(w, 503, "配置管理不可用")
		return
	}
	value := h.cfg.ReadConfig()
	if config, ok := value.(map[string]any); ok && h.cfg.KeyStore != nil {
		active := false
		for _, key := range h.cfg.KeyStore.List() {
			active = active || key.Status == "active"
		}
		config["api_key_configured"], config["api_key_required"], config["api_key_managed"] = active, h.cfg.KeyStore.Required(), true
	}
	respond(w, 200, value)
}
func (h *Handler) saveConfig(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SaveConfig == nil {
		fail(w, 503, "配置管理不可用")
		return
	}
	var body json.RawMessage
	if !decode(w, r, &body) {
		return
	}
	if err := h.cfg.SaveConfig(body); err != nil {
		fail(w, 400, err.Error())
		return
	}
	h.readConfig(w, r)
}
func (h *Handler) restart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Restart == nil {
		fail(w, 409, "请在宿主机手动重启服务")
		return
	}
	respond(w, 202, map[string]any{"ok": true, "message": "正在重启，连接将短暂中断"})
	// Leave time for the response to reach the browser before graceful shutdown.
	time.AfterFunc(500*time.Millisecond, h.cfg.Restart)
}
