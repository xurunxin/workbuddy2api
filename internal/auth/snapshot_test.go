package auth

import "testing"

// TestSnapshotPreservesRealm realm 是**未导出**字段，跨包（catalog/admin）手工构造
// `&Auth{AccessToken:..., Domain:...}` 的拷贝会静默丢掉它，Realm() 随后只能按 domain
// 回落推断——对「显式 realm=global + cn domain」这类 ResolveRealm 明确支持的合法组合，
// 丢 realm 会让账号被判成 cn，模型目录/计费走错域（中国区结果挂在国际区账号名下）。
//
// Snapshot 的存在就是为了让跨包拷贝不再有这个陷阱。本测试是它的回归锚点。
func TestSnapshotPreservesRealm(t *testing.T) {
	t.Parallel()
	a := &Auth{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    42,
		Domain:       "www.codebuddy.cn", // cn domain，但显式 realm=global（显式优先）
		realm:        "global",
		UID:          "u1",
		EnterpriseID: "e1",
		Nickname:     "昵称",
		DeviceToken:  "dt",
		FilePath:     "/tmp/auth.json",
	}
	s := a.Snapshot()
	if s == nil {
		t.Fatal("Snapshot returned nil")
	}
	if got := s.Realm(); got != "global" {
		t.Errorf("snapshot Realm()=%q want global (realm must survive cross-package copy)", got)
	}
	if s.RealmStored() != "global" {
		t.Errorf("snapshot stored realm=%q want global", s.RealmStored())
	}
	// 其余凭证字段完整。
	if s.AccessToken != "at" || s.RefreshToken != "rt" || s.ExpiresAt != 42 {
		t.Errorf("snapshot tokens incomplete: %+v", s)
	}
	if s.Domain != "www.codebuddy.cn" || s.UID != "u1" || s.EnterpriseID != "e1" || s.Nickname != "昵称" {
		t.Errorf("snapshot identity incomplete: %+v", s)
	}
	// DeviceToken 参与出站 X-Device-Token 头，掉落会让风控指纹不完整。
	if s.DeviceToken != "dt" {
		t.Errorf("snapshot device_token=%q want dt", s.DeviceToken)
	}
	// FilePath 刻意不复制：快照只用于读上游，写回必须走原对象（SaveAtomic），
	// 复制 FilePath 会让调用方以为可以拿快照落盘，实际覆盖同一文件却绕过原对象的锁。
	if s.FilePath != "" {
		t.Errorf("snapshot FilePath=%q want empty (snapshots are read-only views)", s.FilePath)
	}
}

// TestSnapshotNilAuth 防御：nil 账号返回 nil（不 panic）。
// catalog.snapshot 在 a==nil 时早退，但 Snapshot 自身也必须是安全的。
func TestSnapshotNilAuth(t *testing.T) {
	t.Parallel()
	var a *Auth
	if got := a.Snapshot(); got != nil {
		t.Errorf("nil Auth Snapshot()=%+v want nil", got)
	}
}

// TestSnapshotIsDetached 快照必须与源对象**脱离**：改写源 token 不影响快照
// （否则「快照防并发改写」的目的一开始就没达成）。
func TestSnapshotIsDetached(t *testing.T) {
	t.Parallel()
	a := &Auth{AccessToken: "old", UID: "u1", realm: "cn"}
	s := a.Snapshot()
	a.Lock()
	a.AccessToken = "new"
	a.Unlock()
	if s.AccessToken != "old" {
		t.Errorf("snapshot AccessToken=%q want old (must be detached)", s.AccessToken)
	}
}
