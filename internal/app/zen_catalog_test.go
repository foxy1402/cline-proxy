package app

import (
	"strings"
	"testing"
)

// 目录可达时价格门是唯一权威：免费集合原样采纳，不掺种子、不看名字后缀。
func TestZenDesiredFromLiveRegistryWins(t *testing.T) {
	free := map[string]bool{
		"big-pickle":                      true, // 无 free 后缀但 cost 0/0
		"mimo-v2.5-free":                  true,
		"muse-spark-1.3-contributor-free": true,
	}
	got, err := zenDesiredFromLive(true, free)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(free) {
		t.Fatalf("desired = %v, want exactly the registry free set %v", got, free)
	}
	for id := range free {
		if !got[id] {
			t.Errorf("registry-free model %q missing from desired", id)
		}
	}
	if len(got) != 3 {
		t.Fatalf("desired = %v, want exactly 3 entries", got)
	}
}

// 目录不可达：报错（调用方保留旧表，种子条目在首次成功同步前仍可服务）。
// 收割机时代的 CLI 成员表兜底已随内嵌 CLI 移除。
func TestZenDesiredFromLiveRegistryDownErrors(t *testing.T) {
	got, err := zenDesiredFromLive(false, nil)
	if err == nil {
		t.Fatal("expected error when registry is unreachable")
	}
	if got != nil {
		t.Fatalf("desired = %v, want nil on error", got)
	}
	if _, err := zenDesiredFromLive(true, map[string]bool{}); err == nil {
		t.Fatal("expected error when registry returned no free models")
	}
}

// live 列表是权威：不在其中的条目一律移除（含种子），而不是只升级来源。
func TestZenPruneRemovesStaleSeedEntries(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{
		"mimo-v2.5-free":         {ID: "mimo-v2.5-free", Source: "seed", Aliases: []string{"mimo"}},
		"deepseek-v4-flash-free": {ID: "deepseek-v4-flash-free", Source: "seed"}, // 已 deprecated
		"big-pickle":             {ID: "big-pickle", Source: "seed"},
	}
	zenAliases = map[string]*ZenModel{"mimo": zenModels["mimo-v2.5-free"]}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	desired := map[string]bool{"mimo-v2.5-free": true, "big-pickle": true}
	pruneZenModelsTo(desired)

	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if _, ok := zenModels["deepseek-v4-flash-free"]; ok {
		t.Error("deprecated seed survived a live sync (should be pruned)")
	}
	if _, ok := zenModels["mimo-v2.5-free"]; !ok {
		t.Error("live model was pruned")
	}
	if _, ok := zenAliases["mimo"]; !ok {
		t.Error("alias of a surviving model must be kept")
	}
}

// 残表防御：live 结果明显偏小（接口抖动）时不得清空。
func TestZenPruneSkipsOnSuspiciouslySmallLiveList(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		zenModels[id] = &ZenModel{ID: id, Source: "seed"}
	}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	pruneZenModelsTo(map[string]bool{"a": true}) // 1 live vs 6 known

	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if len(zenModels) != 6 {
		t.Fatalf("table size = %d, want 6 (small live list must not prune)", len(zenModels))
	}
}

// 种子兜底：没有 live 来源时表里仍是种子（离线可用），且种子里的别名可解析。
func TestZenSeedBootstrapAliasesResolve(t *testing.T) {
	initZenModels()
	m, ok := resolveZenFreeModel("mimo")
	if !ok {
		t.Fatal("alias mimo must resolve to a free zen model")
	}
	if m.ID != "mimo-v2.5-free" {
		t.Fatalf("alias mimo -> %q, want mimo-v2.5-free", m.ID)
	}
	if !strings.HasSuffix(m.ID, "-free") && m.ID != "big-pickle" {
		t.Fatalf("resolved %q is not a free-looking id", m.ID)
	}
}
