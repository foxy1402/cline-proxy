package app

import (
	"os"
	"path/filepath"
	"testing"
)

// 2026-10-30 抓包：免费层路径从 /inference/v1 变成 /inference/openai/v1。
// 这里锁三件事：新默认值、旧默认值的存量迁移、以及目录自愈只在"仍是默认值"
// 时生效（用户显式配置的地址绝不能被远端目录改写）。
const (
	wantBase  = "https://opencode.ai/inference/openai/v1"
	staleBase = "https://opencode.ai/inference/v1"
)

func TestZenAPIBaseIsOpenAIPath(t *testing.T) {
	if zenAPIBase != wantBase {
		t.Fatalf("zenAPIBase = %q, want %q", zenAPIBase, wantBase)
	}
	if defaultZenConfig().BaseURL != wantBase {
		t.Fatalf("default baseURL = %q, want %q", defaultZenConfig().BaseURL, wantBase)
	}
}

// 旧安装的 .zen-config.json 里存的是上一版默认值，必须被迁移到新路径；
// 否则升级后仍然打在短路径上（本次问题的根本形态）。
func TestLoadZenConfigMigratesStaleBase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	path := filepath.Join(dir, ".zen-config.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"key":"public","keys":["public"],"baseURL":"`+staleBase+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := loadZenConfig()
	if cfg.BaseURL != wantBase {
		t.Fatalf("stale baseURL not migrated: got %q, want %q", cfg.BaseURL, wantBase)
	}
}

// 显式配置的自定义地址（自建中转/抓包代理）绝不能被迁移逻辑改掉。
func TestLoadZenConfigKeepsCustomBase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	path := filepath.Join(dir, ".zen-config.json")
	const custom = "https://mirror.example.test/v1"
	if err := os.WriteFile(path, []byte(`{"enabled":true,"key":"public","keys":["public"],"baseURL":"`+custom+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := loadZenConfig()
	if cfg.BaseURL != custom {
		t.Fatalf("custom baseURL rewritten: got %q, want %q", cfg.BaseURL, custom)
	}
}

func TestApplyZenRegistryBase(t *testing.T) {
	// 会经 setZenConfig 写盘并改全局配置：隔离到临时 DATA_DIR，结束后还原。
	t.Setenv("DATA_DIR", t.TempDir())
	saved := getZenConfig()
	t.Cleanup(func() { setZenConfig(saved) })

	const regAPI = "https://opencode.ai/inference/openai/v1"

	// 仍是默认值 → 跟随目录。
	if !applyZenRegistryBase(regAPI, staleBase) {
		t.Error("stale default should be corrected from registry")
	}
	// 已经一致 → 不动（不产生多余的写盘）。
	if applyZenRegistryBase(regAPI, wantBase) {
		t.Error("already-correct base must not be rewritten")
	}
	// 用户显式配置 → 绝不覆盖。
	if applyZenRegistryBase(regAPI, "https://mirror.example.test/v1") {
		t.Error("custom base must never be overwritten by the registry")
	}
	// 目录给空值 → 忽略（不可达/字段缺失时不能让基地址变空）。
	if applyZenRegistryBase("", wantBase) {
		t.Error("empty registry api must be ignored")
	}
	// 非 https、非 opencode.ai、或不在 /inference/ 下 → 忽略：目录是远端数据，
	// 不能让一个被篡改的响应把带 Authorization 的流量导去任意主机，也不能把
	// 基地址指回已失效的 /zen/v1（匿名流量在那里恒 403）。
	for _, bad := range []string{
		"http://opencode.ai/inference/openai/v1",
		"https://evil.example/inference/openai/v1",
		"https://opencode.ai.evil.example/inference/openai/v1",
		"https://opencode.ai/zen/v1",
		"https://opencode.ai/",
	} {
		if applyZenRegistryBase(bad, wantBase) {
			t.Errorf("registry api %q must be rejected", bad)
		}
	}
}
