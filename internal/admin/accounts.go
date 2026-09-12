package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"workbuddy2api/internal/auth"
)

var safeUID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (h *Handler) beginAccount(uid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busy[uid] {
		return false
	}
	h.busy[uid] = true
	return true
}
func (h *Handler) endAccount(uid string) { h.mu.Lock(); delete(h.busy, uid); h.mu.Unlock() }

func (h *Handler) saveAccount(a *auth.Auth) error {
	if !safeUID.MatchString(a.UID) || a.RefreshToken == "" {
		return fmt.Errorf("账号必须包含有效 uid、accessToken 和 refreshToken")
	}
	if !h.beginAccount(a.UID) {
		return fmt.Errorf("账号正在执行操作，请稍后重试")
	}
	defer h.endAccount(a.UID)
	old := h.cfg.Pool.AuthByUID(a.UID)
	if old != nil {
		// Updating credentials on a live Auth pointer races with upstream header reads.
		// Existing accounts can be replaced only after disabling and draining them.
		st, _ := h.cfg.Pool.Status(a.UID)
		if !st.Disabled || st.InFlight > 0 {
			return fmt.Errorf("该账号已存在，请先停用并等待在途请求结束，再重新授权或导入")
		}
		// Old refreshers must finish before the replacement file is installed.
		old.Lock()
		defer old.Unlock()
		a.FilePath = old.FilePath
	} else {
		a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-"+a.UID+".json")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		return fmt.Errorf("凭证目录不可写，请检查数据卷权限")
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("无法保存凭证，请检查数据卷权限")
	}
	// A scheduler may still hold the retired pointer. Its later SaveAtomic must
	// fail instead of overwriting the freshly authorized credentials.
	if old != nil {
		old.FilePath = ""
	}
	h.cfg.Pool.Add(a)
	h.cfg.Pool.ReviveDisabled(a.UID)
	h.cfg.Pool.Flush()
	return nil
}

func (h *Handler) importAccount(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail(w, 400, "凭证格式错误，请使用 workbuddy 账号 JSON 文件内容")
		return
	}
	if err := h.saveAccount(a); err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 201, map[string]any{"uid": a.UID, "nickname": a.Nickname, "status": "added"})
}

func (h *Handler) accountAction(w http.ResponseWriter, r *http.Request) {
	uid, action := r.PathValue("uid"), r.PathValue("action")
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		fail(w, 404, "账号不存在")
		return
	}
	if !h.beginAccount(uid) {
		fail(w, 409, "账号正在执行操作，请稍后重试")
		return
	}
	defer h.endAccount(uid)
	switch action {
	case "disable":
		h.cfg.Pool.Disable(uid, "管理员停用")
	case "enable":
		h.cfg.Pool.ReviveDisabled(uid)
	case "credits":
		// Read a detached credential snapshot so RPC reads cannot race with refresh.
		a.Lock()
		copy := &auth.Auth{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt, Domain: a.Domain, UID: a.UID, EnterpriseID: a.EnterpriseID}
		a.Unlock()
		if copy.NeedsRefresh(0) {
			if err := h.cfg.Upstream.RefreshToken(a); err != nil {
				fail(w, 502, "账号凭证刷新失败，请重新授权账号")
				return
			}
			if err := a.SaveAtomic(); err != nil {
				fail(w, 500, "刷新后的凭证无法保存，请检查凭证卷权限")
				return
			}
			h.cfg.Pool.ClearSessionDead(uid)
			a.Lock()
			copy = &auth.Auth{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt, Domain: a.Domain, UID: a.UID, EnterpriseID: a.EnterpriseID}
			a.Unlock()
		}
		resource, err := h.cfg.Upstream.ResourceUsage(copy)
		if err != nil {
			fail(w, 502, "上游积分查询失败，请稍后重试或重新授权账号")
			return
		}
		h.cfg.Pool.SetCredits(uid, resource.Remain)
		if h.cfg.Usage != nil {
			if err := h.cfg.Usage.SetCredit(uid, resource.Remain, resource.Used); err != nil {
				fail(w, 500, "积分已查询，但统计保存失败，请检查存储权限")
				return
			}
		}
		h.mu.Lock()
		h.creditsChecked[uid] = time.Now()
		h.mu.Unlock()
	default:
		fail(w, 404, "不支持的账号操作")
		return
	}
	h.cfg.Pool.Flush()
	st, _ := h.cfg.Pool.Status(uid)
	respond(w, 200, st)
}
