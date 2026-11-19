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

func (h *Handler) saveAccount(a *auth.Auth, allowReplace bool) error {
	if !safeUID.MatchString(a.UID) || a.RefreshToken == "" {
		return fmt.Errorf("账号必须包含有效 uid、accessToken 和 refreshToken")
	}
	if !h.beginAccount(a.UID) {
		return fmt.Errorf("账号正在执行操作，请稍后重试")
	}
	defer h.endAccount(a.UID)
	old := h.cfg.Pool.AuthByUID(a.UID)
	saved := false
	if old != nil {
		st, _ := h.cfg.Pool.Status(a.UID)
		if !allowReplace {
			// 导入路径的防误覆盖：活跃账号（既没被系统禁用、也没被运维手动摘除）
			// 不允许用一份 JSON 静默替换——那可能是同 UID 的旧文件/别人的文件。
			// 已停用（任一位）的账号仍允许导入恢复，保持原有运维习惯。
			if !st.Disabled && !st.ManualDisabled {
				return fmt.Errorf("该账号仍在选号池中；如需更新凭证，请使用账号行的「重新登录」")
			}
		}
		// 重新登录/覆盖导入：账号可能仍在选号池里，甚至还有在途请求。凭证对象
		// 一旦被替换，在途请求与调度器手里的旧指针就会变成"孤儿"——先确认没有
		// 在途请求，再把旧对象的 FilePath 摘掉，确保它之后的 SaveAtomic 必然失败，
		// 不会把刚写入的新凭证覆盖回旧值。
		//
		// 锁顺序：这里只短暂持有 auth 锁，**不能**跨到 pool 锁——statusOf 等路径
		// 持有 pool 读锁时会取 auth 锁，反向持锁会 ABBA 死锁（旧实现的真实缺陷：
		// 重新登录在账号状态刷新并发时会卡死）。
		if st.InFlight > 0 {
			return fmt.Errorf("账号仍有 %d 个在途请求，请等待请求结束后重试", st.InFlight)
		}
		path := old.FilePathValue()
		if path == "" {
			path = filepath.Join(h.cfg.AuthDir, "workbuddy-"+a.UID+".json")
		}
		a.FilePath = path
		// 重登/覆盖只更换令牌，不丢运维元数据：显式 realm、业务分组、设备 token。
		a.AdoptMetadata(old)
		old.DetachSource()
		defer func() {
			// 保存失败时恢复旧对象的来源，避免一次失败的重新登录把正常续期写回
			// 也一起废掉。保存成功则旧对象永久退役（它之后写回必失败）。
			if !saved {
				old.SetSource(path)
			}
		}()
	} else {
		a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-"+a.UID+".json")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		return fmt.Errorf("凭证目录不可写，请检查数据卷权限")
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("无法保存凭证，请检查数据卷权限")
	}
	saved = true
	// 新凭证进池：Add 对已存在账号只替换凭证对象，保留 credits/冷却/台账等运行态；
	// ReviveDisabled 清系统自动禁用位（重新登录本就为救活 session 死号）。
	// manual_disabled 位不动：运维原有的停用意图由调用方（重新登录流程）恢复。
	h.cfg.Pool.Add(a)
	h.cfg.Pool.ReviveDisabled(a.UID)
	h.cfg.Pool.Flush()
	return nil
}

// accountHold 运维操作的临时摘除现场：把账号从选号池里摘出（manual_disabled），
// 操作结束后恢复到操作前的状态。用于「重新登录」「移除」这类需要等在途请求
// 排空、又不该改变运维原有停用意图的动作。
type accountHold struct {
	uid    string
	active bool // true = 本次操作临时置位，结束时需要清除
}

func (h *Handler) holdAccount(uid, reason string) accountHold {
	prev, _, ok := h.cfg.Pool.ManualDisabledState(uid)
	if !ok {
		return accountHold{}
	}
	if prev {
		// 账号本来就手动停用：已经不在选号池，无需改动，也不要在结束时误清。
		return accountHold{uid: uid}
	}
	if found, _ := h.cfg.Pool.SetManualDisabled(uid, true, reason); !found {
		return accountHold{}
	}
	h.cfg.Pool.Flush()
	return accountHold{uid: uid, active: true}
}

func (h *Handler) releaseAccount(hold accountHold) {
	if !hold.active || hold.uid == "" {
		return
	}
	h.cfg.Pool.SetManualDisabled(hold.uid, false, "")
	h.cfg.Pool.Flush()
}

// credentialFilePath 计算可安全删除的凭证文件路径。
// 只删除 auth_dir 下的 workbuddy*.json：FilePath 可能来自旧版本/手工维护的任意
// 路径，直接 os.Remove 等于给了控制台一个任意文件删除入口（CWE-22）。
func credentialFilePath(authDir, filePath, uid string) string {
	fallback := filepath.Join(authDir, "workbuddy-"+uid+".json")
	if filePath == "" {
		return fallback
	}
	absDir, errDir := filepath.Abs(authDir)
	absFile, errFile := filepath.Abs(filePath)
	if errDir != nil || errFile != nil || filepath.Dir(absFile) != absDir {
		return ""
	}
	if ok, _ := filepath.Match("workbuddy*.json", filepath.Base(absFile)); !ok {
		return ""
	}
	return absFile
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
	// 重新登录流程进行中：凭证即将被 OAuth 结果替换，此时导入会互相覆盖。
	if h.cfg.Pool.Reauthenticating(a.UID) {
		fail(w, 409, "账号正在重新登录，请先取消授权流程或等待完成")
		return
	}
	updated := h.cfg.Pool.AuthByUID(a.UID) != nil
	if err := h.saveAccount(a, false); err != nil {
		fail(w, 400, err.Error())
		return
	}
	status := "added"
	if updated {
		// 覆盖导入等价于「用 JSON 重新登录」：凭证已替换，运行态与手动停用位保留。
		status = "updated"
	}
	respond(w, 201, map[string]any{"uid": a.UID, "nickname": a.Nickname, "status": status})
}

func (h *Handler) removeAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !safeUID.MatchString(uid) {
		fail(w, 404, "账号不存在")
		return
	}
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
	// 正在重新登录的账号不能删：授权流程完成后会把账号重新写回池里，用户会看到
	//「删了又自己回来」。先取消授权流程，语义才明确。
	h.mu.Lock()
	for _, f := range h.logins {
		if f.targetUID == uid && !f.completed {
			h.mu.Unlock()
			fail(w, 409, "该账号正在重新登录，请先取消授权流程再移除")
			return
		}
	}
	h.mu.Unlock()
	hold := h.holdAccount(uid, "删除中")
	defer h.releaseAccount(hold)
	st, _ := h.cfg.Pool.Status(uid)
	if st.InFlight > 0 {
		fail(w, 409, fmt.Sprintf("账号仍有 %d 个在途请求，请等待请求结束后再移除", st.InFlight))
		return
	}
	path := credentialFilePath(h.cfg.AuthDir, a.FilePathValue(), uid)
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fail(w, 500, "凭证文件删除失败，请检查数据卷权限")
			return
		}
	}
	_, busy := h.cfg.Pool.RemoveAccount(uid)
	if busy {
		fail(w, 409, "账号仍有在途请求，请稍后再移除")
		return
	}
	// found=false 时文件可能已删除、auths 目录热加载先行把账号剔除；文件已删即视为
	// 移除成功，不再回 404 误导运维。
	h.cfg.Pool.Flush()
	h.mu.Lock()
	delete(h.creditsChecked, uid)
	h.mu.Unlock()
	respond(w, 200, map[string]any{"uid": uid, "status": "removed"})
}

func (h *Handler) reorderAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UIDs []string `json:"uids"`
	}
	if !decode(w, r, &body) {
		return
	}
	order, err := h.cfg.Pool.Reorder(body.UIDs)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	h.cfg.Pool.Flush()
	respond(w, 200, map[string]any{"order": order, "accounts": h.cfg.Pool.List()})
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
	// 重新登录期间账号被临时摘除且在替换凭证：任何账号级动作（刷新积分会续期
	// token、恢复会提前放回选号池）都可能与替换流程互踩，统一要求先完成/取消。
	if h.cfg.Pool.Reauthenticating(uid) {
		fail(w, 409, "账号正在重新登录，请先完成或取消授权流程")
		return
	}
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
