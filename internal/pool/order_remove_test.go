// order_remove_test.go 账号展示顺序与移除原语的契约：
//   - 排序是持久化的展示顺序（不参与选号权重），未列入的账号按 uid 追加；
//   - 移除有在途请求时必须拒绝（否则 Release/状态记账会落到已删除的 entry 上）。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

func TestReorderAccounts(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "a", RefreshToken: "r"})
	}
	if got := statusUIDs(p.List()); got != "u1,u2,u3" {
		t.Fatalf("default order=%s", got)
	}
	order, err := p.Reorder([]string{"u3", "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := join(order); got != "u3,u1,u2" {
		t.Fatalf("normalized order=%s", got)
	}
	if got := statusUIDs(p.List()); got != "u3,u1,u2" {
		t.Fatalf("list order=%s", got)
	}
	if _, err := p.Reorder([]string{"u3", "nope"}); err == nil {
		t.Fatal("unknown uid accepted")
	}
	if _, err := p.Reorder([]string{"u3", "u3"}); err == nil {
		t.Fatal("duplicate uid accepted")
	}
}

func TestReorderPersistsAcrossReload(t *testing.T) {
	stateFile := t.TempDir() + "/state.json"
	p := New(stateFile)
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "a", RefreshToken: "r"})
	}
	if _, err := p.Reorder([]string{"u2", "u3", "u1"}); err != nil {
		t.Fatal(err)
	}
	p.Flush()
	p.Close()

	p2 := New(stateFile)
	defer p2.Close()
	if got := statusUIDs(p2.List()); got != "u2,u3,u1" {
		t.Fatalf("reloaded order=%s", got)
	}
}

func TestRemoveAccountRefusesInFlight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", RefreshToken: "r"})
	if !p.Acquire("u1") {
		t.Fatal("acquire")
	}
	if found, busy := p.RemoveAccount("u1"); !found || !busy {
		t.Fatalf("found=%v busy=%v want true,true", found, busy)
	}
	if p.AuthByUID("u1") == nil {
		t.Fatal("busy remove deleted the account")
	}
	p.Release("u1")
	if found, busy := p.RemoveAccount("u1"); !found || busy {
		t.Fatalf("found=%v busy=%v want true,false", found, busy)
	}
	if p.AuthByUID("u1") != nil {
		t.Fatal("account still present after remove")
	}
	if found, _ := p.RemoveAccount("u1"); found {
		t.Fatal("second remove should report not found")
	}
}

func TestReauthFlag(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", RefreshToken: "r"})
	if p.Reauthenticating("u1") {
		t.Fatal("fresh pool marked reauth")
	}
	p.BeginReauth("u1")
	if !p.Reauthenticating("u1") {
		t.Fatal("BeginReauth not visible")
	}
	p.EndReauth("u1")
	if p.Reauthenticating("u1") {
		t.Fatal("EndReauth did not clear")
	}
	// 移除账号时顺带清理标记，避免 uid 复用时残留。
	p.BeginReauth("u1")
	if _, busy := p.RemoveAccount("u1"); busy {
		t.Fatal("unexpected busy")
	}
	if p.Reauthenticating("u1") {
		t.Fatal("remove left reauth flag")
	}
}

func statusUIDs(list []Status) string {
	out := make([]string, 0, len(list))
	for _, st := range list {
		out = append(out, st.UID)
	}
	return join(out)
}

func join(parts []string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
