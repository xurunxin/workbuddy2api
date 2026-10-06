// config_testutil_test.go —— 配置测试的共享辅助。
//
// writeCfg 把一段 JSON 写进临时 config 文件并返回路径。budget/alerting/metrics
// 三组配置测试都需要「从文件加载」而非直接构造 Config——只有走 Load() 才能覆盖
// 到「Default() 打底 → JSON 覆盖 → env 覆盖 → normalize 校验」的完整链路，
// 直接构造 Config 会绕过 normalize，测不出非法值是否真的被拒。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCfg 将 content 写入临时目录下的 c.json 并返回其路径。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}
