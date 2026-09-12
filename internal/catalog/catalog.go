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

func snapshot(a *auth.Auth) *auth.Auth {
	a.Lock()
	defer a.Unlock()
	return &auth.Auth{UID: a.UID, Domain: a.Domain, EnterpriseID: a.EnterpriseID,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt}
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

func (e *entry) view(uid, source string) View {
	v := View{AccountUID: uid, Source: source, Stale: source == "stale", Models: make([]upstream.ModelInfo, 0, len(e.models)), Error: e.message}
	if len(e.models) == 0 {
		v.Source = "unavailable"
		return v
	}
	fetched, expires := e.fetched, e.fetched.Add(TTL)
	v.FetchedAt, v.ExpiresAt = &fetched, &expires
	for _, m := range e.models {
		m.Efforts = append([]string{}, m.Efforts...)
		m.Tags = append([]string{}, m.Tags...)
		if m.CreditMultiplier != nil {
			n := *m.CreditMultiplier
			m.CreditMultiplier = &n
		}
		v.Models = append(v.Models, m)
	}
	return v
}
