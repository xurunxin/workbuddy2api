package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Infrastructure paths and admin credentials remain deployment-owned. Only
// these gateway settings may be persisted from the remote console.
var editableKeys = map[string]bool{"api_key": true, "server": true, "cooldown": true, "schedule": true, "upstream": true, "features": true, "prompt": true, "pool": true, "session_sticky": true}

func managedPath(c *Config) string {
	return filepath.Join(filepath.Dir(c.StateFile), "admin-config.json")
}

func managedObject(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("配置必须是 JSON 对象")
	}
	for k, v := range m {
		if !editableKeys[k] {
			return nil, fmt.Errorf("不可编辑的配置项：%s", k)
		}
		if v == nil {
			return nil, fmt.Errorf("配置项 %s 不能为 null", k)
		}
	}
	if containsNull(m) {
		return nil, fmt.Errorf("配置值不能为 null")
	}
	return m, nil
}
func containsNull(m map[string]any) bool {
	for _, v := range m {
		if v == nil {
			return true
		}
		if child, ok := v.(map[string]any); ok && containsNull(child) {
			return true
		}
	}
	return false
}

func loadManagedConfig(c *Config) error {
	raw, err := os.ReadFile(managedPath(c))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取管理配置失败：%w", err)
	}
	if _, err := managedObject(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return fmt.Errorf("管理配置无效：%w", err)
	}
	return nil
}

type configManager struct {
	mu      sync.Mutex
	active  *Config
	overlay map[string]any
	path    string
}

func newConfigManager(c *Config) (*configManager, error) {
	m := &configManager{active: c, overlay: map[string]any{}, path: managedPath(c)}
	raw, err := os.ReadFile(m.path)
	if err == nil {
		m.overlay, err = managedObject(raw)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return m, nil
}
func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		child, ok := v.(map[string]any)
		old, oldOK := dst[k].(map[string]any)
		if ok && oldOK {
			mergeMap(old, child)
		} else {
			dst[k] = v
		}
	}
}
func cloneMap(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	return c
}
func editableConfig(c *Config) map[string]any {
	raw, _ := json.Marshal(c)
	var all map[string]any
	_ = json.Unmarshal(raw, &all)
	for k := range all {
		if !editableKeys[k] || k == "api_key" {
			delete(all, k)
		}
	}
	return all
}
func (m *configManager) candidate(overlay map[string]any) (*Config, error) {
	raw, _ := json.Marshal(m.active)
	c := Default()
	_ = json.Unmarshal(raw, c)
	raw, err := json.Marshal(overlay)
	if err != nil {
		return nil, fmt.Errorf("配置序列化失败")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return nil, fmt.Errorf("配置字段或类型无效：%w", err)
	}
	// Validate the actual edit before applying environmental overrides so bad
	// values cannot hide behind an env var and break a later deployment.
	if err := validateManaged(c); err != nil {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}
func validateManaged(c *Config) error {
	if c.Server.MaxBodyMB < 1 || c.Server.MaxBodyMB > 1024 {
		return fmt.Errorf("server.max_body_mb 必须为 1–1024")
	}
	if c.Pool.MaxInFlight < 0 || c.Pool.MaxInFlight > 10000 || c.Pool.BreakerThreshold < 1 || c.Pool.IdleWeightPerHour < 0 || c.Pool.IdleWeightMax < 0 {
		return fmt.Errorf("账号池参数超出有效范围")
	}
	if c.Upstream.TimeoutSeconds < 1 || c.Upstream.TimeoutSeconds > 3600 || c.Upstream.HeaderTimeoutSeconds < 0 || c.Upstream.HeaderTimeoutSeconds > 3600 || c.Upstream.IdleTimeoutSeconds < 0 || c.Upstream.IdleTimeoutSeconds > 86400 {
		return fmt.Errorf("上游超时参数超出有效范围")
	}
	if strings.ContainsAny(c.Upstream.UserAgent, "\r\n") {
		return fmt.Errorf("User-Agent 不能包含换行")
	}
	for key, value := range map[string]string{"cooldown.soft_rate": c.Cooldown.SoftRate, "cooldown.soft_rate_max": c.Cooldown.SoftRateMax, "pool.breaker_cooldown": c.Pool.BreakerCooldown, "pool.breaker_cooldown_max": c.Pool.BreakerCooldownMax, "session_sticky.ttl": c.SessionSticky.TTL, "session_sticky.gc_interval": c.SessionSticky.GCInterval} {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s 必须为正的时长，例如 30m", key)
		}
	}
	return nil
}
func (m *configManager) read() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.candidate(m.overlay)
	if err != nil {
		c = m.active
	}
	var env []string
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if strings.HasPrefix(name, "WB2A_") {
			env = append(env, name)
		}
	}
	sort.Strings(env)
	return map[string]any{"config": editableConfig(c), "effective_config": editableConfig(m.active), "api_key_configured": c.APIKey != "", "pending_restart": !reflect.DeepEqual(c, m.active), "environment_overrides": env, "storage": "data/admin-config.json", "restart_supported": true}
}
func (m *configManager) save(raw json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	patch, err := managedObject(raw)
	if err != nil {
		return err
	}
	if _, exists := patch["api_key"]; exists {
		return fmt.Errorf("API Key 已独立管理，请在 API Key 页面创建或废弃密钥")
	}
	candidate := cloneMap(m.overlay)
	mergeMap(candidate, patch)
	if _, err := m.candidate(candidate); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return fmt.Errorf("配置目录不可写，请检查 data 卷权限")
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".admin-config-*")
	if err != nil {
		return fmt.Errorf("配置目录不可写，请检查 data 卷权限")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(encoded)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, m.path)
	}
	if err != nil {
		return fmt.Errorf("配置保存失败，请检查 data 卷权限")
	}
	m.overlay = candidate
	return nil
}
