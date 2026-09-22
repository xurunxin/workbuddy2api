package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

var safeUID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// accountActionReason 读可选的 {"reason": "..."} 请求体（临时停用原因，运维留痕用）。
// 无体 / 非 JSON / 无该字段都回落默认文案——无体是最常见的调用形态（curl / 脚本），
// 不应因体缺失而拒绝。长度截断到 120 字符，避免超长文本撑爆账号状态展示。
func accountActionReason(r *http.Request) string {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return "控制台临时停用"
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "控制台临时停用"
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		return "控制台临时停用"
	}
	if runes := []rune(reason); len(runes) > 120 {
		reason = string(runes[:120])
	}
	return reason
}

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
	// ---- 上游运维端点（issue #138/#118）的控制台等价实现 ----------------
	//
	// 上游把「临时停用/恢复/复活」开在 /admin/accounts/{uid}/{disable,enable,revive}
	// 上，用 api_key 鉴权；本 fork 的控制台占用了整个 /admin/ 前缀且路由更具体，
	// 那些端点在本进程内不可达。故按控制台自己的会话鉴权等价实现，语义与 pool
	// 原语逐一对应：
	//   manual_disable → SetManualDisabled(true, reason)  「对话流量摘除」：
	//                    账号留在池里，签到/保活/排程照常，只是不参与选号
	//   manual_enable  → SetManualDisabled(false, "")     只解运维意图
	//   revive         → ReviveDisabled                   只解系统自动禁用
	// 手动位与自动位正交：两个都清空账号才回到选号池。
	case "manual_disable":
		if found, _ := h.cfg.Pool.SetManualDisabled(uid, true, accountActionReason(r)); !found {
			fail(w, 404, "账号不存在")
			return
		}
	case "manual_enable":
		if found, _ := h.cfg.Pool.SetManualDisabled(uid, false, ""); !found {
			fail(w, 404, "账号不存在")
			return
		}
	case "revive":
		h.cfg.Pool.ReviveDisabled(uid)
	// ---- 历史动作：保留可用（老前端/脚本），语义已被上面三个更精确的动作取代 --
	// disable 走的是**自动禁用**位（Disable 带原因，签到解冻/refresh 等路径可能把它
	// 自动撤销）；只想摘对话流量请用 manual_disable。
	case "disable":
		h.cfg.Pool.Disable(uid, "管理员停用")
	case "enable":
		h.cfg.Pool.ReviveDisabled(uid)
	case "credits":
		// Read a detached credential snapshot so RPC reads cannot race with refresh.
		// 走 auth.Snapshot 而非手工构造 Auth：后者会丢掉未导出的 realm，让
		// 「显式 realm=global + cn domain」的账号被判成 cn 而查错域的积分。
		copy := a.Snapshot()
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
			copy = a.Snapshot()
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
