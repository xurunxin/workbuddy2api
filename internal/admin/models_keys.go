package admin

import (
	"errors"
	"net/http"

	"workbuddy2api/internal/accesskey"
)

func (h *Handler) accountModels(w http.ResponseWriter, r *http.Request) {
	force := r.Method == http.MethodPost
	if force && !decode(w, r, &struct{}{}) {
		return
	}
	uid := r.PathValue("uid")
	if !h.beginAccount(uid) {
		fail(w, 409, "账号正在执行操作，请稍后重试")
		return
	}
	defer h.endAccount(uid)
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		fail(w, 404, "账号不存在")
		return
	}
	v, err := h.cfg.Catalog.Get(r.Context(), a, force)
	if err != nil && len(v.Models) == 0 {
		fail(w, 503, err.Error())
		return
	}
	respond(w, 200, v)
}

func (h *Handler) keysAvailable(w http.ResponseWriter) bool {
	if h.cfg.KeyStore == nil {
		fail(w, 503, "API Key 管理尚未初始化")
		return false
	}
	return true
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	if !h.keysAvailable(w) {
		return
	}
	respond(w, 200, map[string]any{"keys": h.cfg.KeyStore.List(), "authentication_required": h.cfg.KeyStore.Required(), "max_keys": accesskey.MaxKeys})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysAvailable(w) {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	v, secret, err := h.cfg.KeyStore.Create(input.Name)
	if errors.Is(err, accesskey.ErrInvalidName) {
		fail(w, 400, "请填写 1–80 个字符的密钥名称")
		return
	}
	if errors.Is(err, accesskey.ErrLimitReached) {
		fail(w, 409, "API Key 记录数量已达上限")
		return
	}
	if err != nil {
		fail(w, 500, "密钥保存失败，请检查 data 卷权限后重试")
		return
	}
	respond(w, 201, map[string]any{"key": v, "secret": secret})
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysAvailable(w) || !decode(w, r, &struct{}{}) {
		return
	}
	err := h.cfg.KeyStore.Revoke(r.PathValue("id"))
	if errors.Is(err, accesskey.ErrNotFound) {
		fail(w, 404, "API Key 不存在")
		return
	}
	if err != nil {
		fail(w, 500, "密钥废弃状态保存失败，请检查 data 卷权限后重试")
		return
	}
	respond(w, 200, map[string]any{"ok": true})
}
