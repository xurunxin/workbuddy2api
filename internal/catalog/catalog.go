// Package catalog caches authenticated model metadata per account. It never
// treats a metadata lookup failure as a chat failure or supplies guessed prices.
package catalog

import (
	"context"
	"errors"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const TTL = 15 * time.Minute
const failureCooldown = time.Minute

type View struct {
	AccountUID string               `json:"account_uid"`
	Realm      string               `json:"realm"`
	Source     string               `json:"source"`
	FetchedAt  *time.Time           `json:"fetched_at"`
	ExpiresAt  *time.Time           `json:"expires_at"`
	Stale      bool                 `json:"stale"`
	Models     []upstream.ModelInfo `json:"models"`
	Error      string               `json:"error,omitempty"`
}

type entry struct {
	mu      sync.Mutex
	account *auth.Auth
	models  []upstream.ModelInfo
	fetched time.Time
	failed  time.Time
	message string
}

type Service struct {
	mu      sync.Mutex
	entries map[string]*entry
	up      *upstream.Client
}

func New(up *upstream.Client) *Service {
	return &Service{up: up, entries: make(map[string]*entry)}
}

// snapshot 返回凭证的脱离锁拷贝（auth.Auth.Snapshot）。
//
// 历史实现手工构造 `&Auth{UID, Domain, EnterpriseID, AccessToken, RefreshToken,
// ExpiresAt}`，**丢掉了未导出的 realm** → Realm() 退化为按 domain 推断：显式
// realm=global 但 domain 为 cn 的账号（ResolveRealm 明确支持的合法组合）会被判成
// cn，模型目录按 CN 上游拉取却挂在 global 账号名下——正是「国际区模型混入」的
// 一类根因。统一改走 Snapshot 后 realm/DeviceToken 一并保留。
func snapshot(a *auth.Auth) *auth.Auth {
	if a == nil {
		return nil
	}
	return a.Snapshot()
}

// Get returns a cached snapshot, or refreshes on expiry/explicit request. A new
// Auth pointer after reauthorization invalidates the previous account catalog.
// Errors contain only fixed public messages, never upstream bodies or tokens.
func (s *Service) Get(ctx context.Context, a *auth.Auth, force bool) (View, error) {
	if a == nil {
		return View{}, errors.New("账号不存在")
	}
	copy := snapshot(a)
	s.mu.Lock()
	e := s.entries[copy.UID]
	if e == nil || e.account != a {
		e = &entry{account: a}
		s.entries[copy.UID] = e
	}
	s.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	if !force && !e.failed.IsZero() && now.Sub(e.failed) < failureCooldown {
		return e.view(copy.UID, "stale"), errors.New(e.message)
	}
	if !force && len(e.models) > 0 && now.Sub(e.fetched) < TTL {
		return e.view(copy.UID, "cache"), nil
	}
	if ctx.Err() != nil {
		return e.view(copy.UID, "stale"), ctx.Err()
	}
	message := ""
	copy = snapshot(a)
	if s.up == nil {
		message = "模型服务不可用"
	} else if copy.NeedsRefresh(0) {
		if err := s.up.RefreshToken(a); err != nil {
			message = "账号凭证刷新失败，请重新授权账号"
		} else if err := a.SaveAtomic(); err != nil {
			message = "刷新后的凭证无法保存，请检查凭证卷权限"
		} else {
			copy = snapshot(a)
		}
	}
	var models []upstream.ModelInfo
	if message == "" {
		var err error
		models, err = s.up.FetchModelsContext(ctx, copy)
		if err != nil {
			message = "上游模型目录查询失败，请稍后刷新或重新授权账号"
		}
	}
	if message != "" {
		e.failed, e.message = time.Now().UTC(), message
		return e.view(copy.UID, "stale"), errors.New(message)
	}
	e.models, e.fetched, e.failed, e.message = models, time.Now().UTC(), time.Time{}, ""
	return e.view(copy.UID, "upstream"), nil
}

// view 组装对外视图。realm 取账号**当前**的 Realm()（而非拉取时冻结值）：
// 账号 realm 可被逃生门或重新授权改写，展示层应反映当下事实，避免「标记 cn 却
// 实际走 global 端点」的陈旧标签。账号为 nil（防御分支）时空串。
func (e *entry) view(uid, source string) View {
	realm := ""
	if e.account != nil {
		realm = e.account.Realm()
	}
	v := View{AccountUID: uid, Realm: realm, Source: source, Stale: source == "stale", Models: make([]upstream.ModelInfo, 0, len(e.models)), Error: e.message}
	if len(e.models) == 0 {
		v.Source = "unavailable"
		return v
	}
	fetched, expires := e.fetched, e.fetched.Add(TTL)
	v.FetchedAt, v.ExpiresAt = &fetched, &expires
	for _, m := range e.models {
		m.Efforts = append([]string{}, m.Efforts...)
		m.Tags = append([]string{}, m.Tags...)
		m.Attachments = append([]string{}, m.Attachments...)
		m.ContextWindowTiers = append([]int64{}, m.ContextWindowTiers...)
		if m.CreditMultiplier != nil {
			n := *m.CreditMultiplier
			m.CreditMultiplier = &n
		}
		v.Models = append(v.Models, m)
	}
	return v
}
