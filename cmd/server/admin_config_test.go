package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 本文件覆盖 "admin" 配置段的解析与校验。
//
// 历史：上游曾在同一段下引入 enabled 开关（api_key 鉴权的
// /admin/accounts/{uid}/{disable,enable,revive} 运维端点）。本 fork 的内嵌管理
// 控制台占用整个 /admin/ 前缀，二者同路径无法并存，故统一到控制台后该开关退役
// （连同 WB2A_ADMIN_ENABLED 覆盖与配套 fail-fast）。此处锁住的是控制台自身仍需
// 保证的行为：password/secure_cookie 能读到，且弱口令被 normalize 拦下。

// TestAdminPasswordLoaded 控制台口令与 secure_cookie 从 config 正常读入。
func TestAdminPasswordLoaded(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"password":"strong-password-1","secure_cookie":true}}`), 0o600)

	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Admin.Password != "strong-password-1" {
		t.Errorf("admin.password=%q", c.Admin.Password)
	}
	if !c.Admin.SecureCookie {
		t.Error("admin.secure_cookie 应为 true")
	}
}

// TestAdminShortPasswordRejected 弱口令（<12 位）必须拒绝启动——控制台是
// 公网可达的管理面，弱口令等于没有鉴权。
func TestAdminShortPasswordRejected(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"password":"short"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("admin.password 少于 12 位应拒绝启动")
	}
}

// TestAdminEmptyPasswordAllowed 未配置口令 = 控制台未启用（登录端点返回 503），
// 不是启动错误——保持「不配也能跑」的现状语义。
func TestAdminEmptyPasswordAllowed(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","admin":{"password":""}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("空口令（控制台未启用）应通过: %v", err)
	}
}

// TestAdminRetiredEnabledKeyIgnored 退役的 admin.enabled 出现在 config 里应被
// **静默忽略**而非报错：老部署升级不该因为一个已退役的开关起不来。
func TestAdminRetiredEnabledKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":true,"password":"strong-password-1"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("退役键不应导致启动失败: %v", err)
	}
	if c.Admin.Password != "strong-password-1" {
		t.Errorf("同段其他键应正常读入, password=%q", c.Admin.Password)
	}
}
