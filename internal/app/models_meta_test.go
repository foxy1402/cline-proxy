package app

import (
	"reflect"
	"testing"
)

// 别名回连的全部规则都在 resolveMetaID 里，这是本次改动的核心风险点：
// 免费层 id 在 /ai/cline/models 里不存在，连错就会给客户端错误的上下文窗口。
func TestResolveMetaID(t *testing.T) {
	entries := []clineMetaEntry{
		{ID: "nvidia/nemotron-3.5-super-vl"},
		{ID: "nvidia/nemotron-3.5-super-vl:free"},
		{ID: "upstage/solar-mini4"},
		{ID: "nvidia/nemotron-3.5-lightning"},
		{ID: "nvidia/nemotron-3.5-lightning:free"},
		{ID: "deepseek/deepseek-v4.1-flash"},
		{ID: "deepseek/deepseek-v4.1-flash:batch"},
	}
	idx := metaCandidateIndex(entries)

	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		// 真实的三条免费别名（2026-11-08 实测在册）
		{"cline-free alias -> paid base preferred over :free", "cline-free/nemotron-3.5-super-vl", "nvidia/nemotron-3.5-super-vl", true},
		{"cline-free alias to single candidate", "cline-free/solar-mini4", "upstage/solar-mini4", true},
		{"-free suffix alias strips suffix", "nvidia/nemotron-3.5-lightning-free", "nvidia/nemotron-3.5-lightning", true},
		// 唯一候选时直接命中，即使自带 :free 之外的后缀歧义
		{"exact alias match with colon variant present", "deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash", true},
		// 拼不出候选的必须放弃，不能瞎连
		{"unknown model", "cline-free/nonexistent-model", "", false},
		{"empty", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveMetaID(tc.input, idx)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("resolveMetaID(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// 歧义时优先非 :free 条目：免费别名上游就是该模型本身，:free 变体是限流版本，
// 参数上限通常更小，用它会低估能力。
func TestResolveMetaIDPrefersNonFreeVariant(t *testing.T) {
	idx := metaCandidateIndex([]clineMetaEntry{
		{ID: "vendor/thing:free"},
		{ID: "vendor/thing"},
	})
	got, ok := resolveMetaID("cline-free/thing", idx)
	if !ok || got != "vendor/thing" {
		t.Fatalf("got (%q,%v), want vendor/thing", got, ok)
	}
}

func TestReasoningEffortsFor(t *testing.T) {
	cases := []struct {
		name string
		params []string
		want []string
	}{
		{"no reasoning support", []string{"tools", "temperature"}, nil},
		{"reasoning param", []string{"reasoning", "tools"}, []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"include_reasoning param", []string{"include_reasoning"}, []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"reasoning_effort param", []string{"reasoning_effort"}, []string{"minimal", "low", "medium", "high", "xhigh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reasoningEffortsFor(&clineMetaEntry{SupportedParameters: tc.params})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if got := reasoningEffortsFor(nil); got != nil {
		t.Fatalf("nil entry should yield nil, got %v", got)
	}
}

// metaCandidateIndex 的末段/冒号剥离决定回连能否命中。
func TestMetaCandidateIndexSegments(t *testing.T) {
	idx := metaCandidateIndex([]clineMetaEntry{
		{ID: "a/b-thing"},
		{ID: "a/b-thing:free"},
		{ID: "noslash"},
	})
	if len(idx["b-thing"]) != 2 {
		t.Fatalf("b-thing should have 2 candidates, got %v", idx["b-thing"])
	}
	if len(idx["noslash"]) != 1 {
		t.Fatalf("slash-less id should index under itself, got %v", idx["noslash"])
	}
}

// applyClineMetaLocked 只补齐元数据、不动 id/provider/成本，且缺元数据的条目保留旧值。
func TestApplyClineMetaLockedEnrichesAndPreserves(t *testing.T) {
	modelsMu.Lock()
	oldCache := modelsCache
	modelsCache = map[string]*ModelInfo{
		"cline-free/solar-mini4": {ID: "cline-free/solar-mini4", Provider: "cline-free", Cost: "free", Status: ModelActive},
		"cline-free/gone":        {ID: "cline-free/gone", Provider: "cline-free", Cost: "free", Status: ModelActive,
			ContextLength: 12345, MetaSource: "stale/old"}, // 无元数据可连 -> 保留旧值
	}
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsCache = oldCache
		modelsMu.Unlock()
	}()

	entries := []clineMetaEntry{{
		ID:              "upstage/solar-mini4",
		Name:            "Solar Mini 4",
		ContextLen:      524288,
		Architecture:    struct {
			Modality        string   `json:"modality"`
			InputModalities []string `json:"input_modalities"`
			OutputModality  []string `json:"output_modalities"`
		}{Modality: "text->text", InputModalities: []string{"text"}, OutputModality: []string{"text"}},
		SupportedParameters: []string{"tools", "structured_outputs", "reasoning_effort"},
		TopProvider: struct {
			ContextLength       int `json:"context_length"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}{ContextLength: 524288, MaxCompletionTokens: 131072},
	}}

	modelsMu.Lock()
	n := applyClineMetaLocked(entries)
	m := modelsCache["cline-free/solar-mini4"]
	gone := modelsCache["cline-free/gone"]
	modelsMu.Unlock()

	if n != 1 {
		t.Fatalf("enriched = %d, want 1 (only solar-mini4 has a candidate)", n)
	}
	if m.ContextLength != 524288 || m.MaxOutputTokens != 131072 {
		t.Fatalf("ctx/out = %d/%d, want 524288/131072", m.ContextLength, m.MaxOutputTokens)
	}
	if m.MetaSource != "upstage/solar-mini4" {
		t.Fatalf("metaSource = %q", m.MetaSource)
	}
	if !m.SupportsReasoning || len(m.ReasoningEfforts) == 0 {
		t.Fatalf("reasoning not detected: %+v", m)
	}
	if !m.SupportsTools || !m.SupportsStructuredOutput {
		t.Fatalf("tools/structured not detected: %+v", m)
	}
	// 元数据不改身份字段
	if m.Provider != "cline-free" || m.Cost != "free" || m.ID != "cline-free/solar-mini4" {
		t.Fatalf("identity fields mutated: %+v", m)
	}
	// 连不上的条目保留旧值，不被清空
	if gone.ContextLength != 12345 || gone.MetaSource != "stale/old" {
		t.Fatalf("unmatched entry lost its previous metadata: %+v", gone)
	}
}

// 空元数据（拉取失败）必须是无操作，绝不能清空已有字段。
func TestApplyClineMetaLockedEmptyIsNoop(t *testing.T) {
	modelsMu.Lock()
	oldCache := modelsCache
	modelsCache = map[string]*ModelInfo{
		"cline-free/solar-mini4": {ID: "cline-free/solar-mini4", ContextLength: 999},
	}
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsCache = oldCache
		modelsMu.Unlock()
	}()

	modelsMu.Lock()
	n := applyClineMetaLocked(nil)
	ctx := modelsCache["cline-free/solar-mini4"].ContextLength
	modelsMu.Unlock()

	if n != 0 || ctx != 999 {
		t.Fatalf("n=%d ctx=%d, want 0/999", n, ctx)
	}
}

// 回归：free feed 收缩到比缓存少一半以下时，必须仍然修剪掉死模型。
// 旧的比例守护（live*2 >= cached）会永久跳过修剪，正是死模型长期挂在
// 面板上的根因。
func TestPruneModelsLockedShrunkFeedStillPrunes(t *testing.T) {
	modelsMu.Lock()
	oldCache := modelsCache
	// 8 个缓存，其中只有 3 个还在 live feed 里 —— 正是 2026-11-08 的现场
	modelsCache = map[string]*ModelInfo{
		"cline-free/nemotron-3.5-super-vl": {ID: "cline-free/nemotron-3.5-super-vl"},
		"cline-free/solar-mini4":           {ID: "cline-free/solar-mini4"},
		"nvidia/nemotron-3.5-lightning-free": {ID: "nvidia/nemotron-3.5-lightning-free"},
		"cline-free/deepseek-v4.1-flash":   {ID: "cline-free/deepseek-v4.1-flash"},   // 404 死模型
		"cline-free/muse-spark-1.3-contributor": {ID: "cline-free/muse-spark-1.3-contributor"}, // 404 死模型
		"cline-free/solar-pro4":            {ID: "cline-free/solar-pro4"},            // 404 死模型
		"z-ai/glm-5.3-flash":               {ID: "z-ai/glm-5.3-flash"},
		"poolside/laguna-s-2.1:free":       {ID: "poolside/laguna-s-2.1:free"},
	}
	live := map[string]bool{
		"cline-free/nemotron-3.5-super-vl":   true,
		"cline-free/solar-mini4":             true,
		"nvidia/nemotron-3.5-lightning-free": true,
	}
	pruned := pruneModelsLocked(live, true)
	n := len(modelsCache)
	_, deadKept := modelsCache["cline-free/deepseek-v4.1-flash"]
	modelsCache = oldCache
	modelsMu.Unlock()

	if pruned != 5 || n != 3 {
		t.Fatalf("pruned=%d remaining=%d, want 5/3", pruned, n)
	}
	if deadKept {
		t.Fatal("dead model survived pruning after feed shrank")
	}
}

// 空 feed 是唯一的跳过条件：不能因为一次空/残缺响应清空整个列表。
func TestPruneModelsLockedEmptyFeedSkips(t *testing.T) {
	modelsMu.Lock()
	oldCache := modelsCache
	modelsCache = map[string]*ModelInfo{
		"cline-free/solar-mini4": {ID: "cline-free/solar-mini4"},
	}
	pruned := pruneModelsLocked(map[string]bool{}, true)
	n := len(modelsCache)
	modelsCache = oldCache
	modelsMu.Unlock()

	if pruned != 0 || n != 1 {
		t.Fatalf("pruned=%d remaining=%d, want 0/1 (empty feed must not wipe the list)", pruned, n)
	}
}

// isZeroPrice：价格是字符串，空串表示字段缺失——绝不能当成免费，
// 否则任何没填价的条目都会被误收进免费列表。
func TestIsZeroPrice(t *testing.T) {
	cases := map[string]bool{
		"0": true, "0.0": true, "0.00": true,
		"0.00000008": false, "1": false,
		"": false, "abc": false, " ": false,
	}
	for in, want := range cases {
		if got := isZeroPrice(in); got != want {
			t.Fatalf("isZeroPrice(%q) = %v, want %v", in, got, want)
		}
	}
}

// isFreeChatModel 是"免费全集"的判据。三个条件缺一不可：
// 双向零价、有 text 输出、声明支持 tools。
func TestIsFreeChatModel(t *testing.T) {
	mk := func(prompt, completion string, outMods, params []string) *clineMetaEntry {
		e := &clineMetaEntry{SupportedParameters: params}
		e.Pricing.Prompt = prompt
		e.Pricing.Completion = completion
		e.Architecture.OutputModality = outMods
		return e
	}
	textTools := []string{"tools", "temperature"}

	cases := []struct {
		name string
		e    *clineMetaEntry
		want bool
	}{
		{"free chat model", mk("0", "0", []string{"text"}, textTools), true},
		{"paid chat model", mk("0.0000003", "0.000001", []string{"text"}, textTools), false},
		{"free but audio-only output (lyria)", mk("0", "0", []string{"text", "audio"}, nil), false},
		{"free chat but no tools", mk("0", "0", []string{"text"}, []string{"temperature"}), false},
		{"free but completion priced", mk("0", "0.000001", []string{"text"}, textTools), false},
		{"missing prompt price is not free", mk("", "", []string{"text"}, textTools), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.isFreeChatModel(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// 免费全集必须包含 catalog 里标价 0 的模型，而不只是 recommended 的 free 组。
// 用户实测 Beam 1.0 (Free) 在 CLI 里能正常聊天，而它从不出现在 free 组里。
func TestLocalFreeModelsUnionBothSources(t *testing.T) {
	recommended := []string{"cline-free/nemotron-3.5-super-vl", "cline-free/solar-mini4"}
	catalog := []clineMetaEntry{
		{ID: "reflection/beam-free", Name: "Beam 1.0 (Free)"},
		{ID: "recursive/flint-v1.0:free", Name: "Recursive: Flint (free)"},
		{ID: "thinkingmachines/inkling-small", Name: "Inkling Small"},
		{ID: "google/lyria-3-pro-preview", Name: "Lyria"},
		{ID: "some/paid-model", Name: "Paid"},
	}
	catalog[0].Pricing.Prompt, catalog[0].Pricing.Completion = "0", "0"
	catalog[0].Architecture.OutputModality = []string{"text"}
	catalog[0].SupportedParameters = []string{"tools"}
	catalog[1].Pricing.Prompt, catalog[1].Pricing.Completion = "0", "0"
	catalog[1].Architecture.OutputModality = []string{"text"}
	catalog[1].SupportedParameters = []string{"tools"}
	catalog[2].Pricing.Prompt, catalog[2].Pricing.Completion = "0.00000045", "0.000001"
	catalog[2].Architecture.OutputModality = []string{"text"}
	catalog[2].SupportedParameters = []string{"tools"}
	catalog[3].Pricing.Prompt, catalog[3].Pricing.Completion = "0", "0"
	catalog[3].Architecture.OutputModality = []string{"text", "audio"}
	catalog[4].Pricing.Prompt = "0.00001"

	var got []string
	for _, id := range recommended {
		got = append(got, id)
	}
	for i := range catalog {
		if catalog[i].isFreeChatModel() {
			got = append(got, catalog[i].ID)
		}
	}

	want := map[string]bool{
		"cline-free/nemotron-3.5-super-vl": true, // from recommended
		"cline-free/solar-mini4":           true, // from recommended
		"reflection/beam-free":             true, // from catalog, zero-priced
		"recursive/flint-v1.0:free":        true, // from catalog, zero-priced
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %v, want exactly %d entries", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected model in free set: %q (all=%v)", id, got)
		}
	}
}

// 回归：:free 条目也曾回连失败。索引键剥掉 ":variant"，查询也必须剥，
// 否则 liquid/lfm-3-1.7b:free 永远命中不了 lfm-3-1.7b。
func TestResolveMetaIDHandlesColonVariant(t *testing.T) {
	idx := metaCandidateIndex([]clineMetaEntry{
		{ID: "liquid/lfm-3-1.7b"},
		{ID: "liquid/lfm-3-1.7b:free"},
		{ID: "cohere/north-mini-code:free"},
	})
	// 精确 id 在 catalog 里，优先返回它自己
	if got, ok := resolveMetaID("liquid/lfm-3-1.7b:free", idx); !ok || got != "liquid/lfm-3-1.7b:free" {
		t.Fatalf("got (%q,%v), want liquid/lfm-3-1.7b:free", got, ok)
	}
	// 只有 :free 一条时也要能回连
	if got, ok := resolveMetaID("cohere/north-mini-code:free", idx); !ok || got != "cohere/north-mini-code:free" {
		t.Fatalf("got (%q,%v), want cohere/north-mini-code:free", got, ok)
	}
}

// extraFreeModels 是 catalog 完全缺失的免费模型兜底。beam-free 用户实测
// 在 CLI 里能正常对话，但服务端 catalog 查不到它 —— 只靠 catalog 会误删。
func TestExtraFreeModelsCoverCatalogGaps(t *testing.T) {
	want := map[string]bool{"reflection/beam-free": true, "poolside/laguna-s-2.1-free": true}
	for _, e := range extraFreeModels {
		if !want[e.ID] {
			t.Fatalf("unexpected extra free model %q", e.ID)
		}
		delete(want, e.ID)
		if e.ContextLength <= 0 || e.MaxOutputTokens <= 0 {
			t.Fatalf("%q missing metadata: ctx=%d out=%d", e.ID, e.ContextLength, e.MaxOutputTokens)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing extra free models: %v", want)
	}
}

// 自愈：feed 说某个模型免费，不代表现在真能调通。19 个标价 0 的模型里实测
// 有 4 个是坏的（gemma×2 上游 500、laguna provider 挂、crouton 404），这些
// 在 feed 里全都正常列着。连续失败达阈值就下架冷却。
func TestModelFailureMarksCooldown(t *testing.T) {
	modelsMu.Lock()
	old := modelsCache
	modelsCache = map[string]*ModelInfo{"m/dead": {ID: "m/dead", Status: ModelActive}}
	modelsMu.Unlock()
	defer func() { modelsMu.Lock(); modelsCache = old; modelsMu.Unlock() }()

	for i := 1; i <= modelFailThreshold; i++ {
		recordModelFailure("m/dead", 500, `{"error":"failed to create stream: boom"}`)
	}
	modelsMu.Lock()
	m := modelsCache["m/dead"]
	inCooldown := modelInCooldown(m)
	streak := m.FailStreak
	modelsMu.Unlock()
	if !inCooldown || streak < modelFailThreshold {
		t.Fatalf("expected cooldown after threshold: streak=%d cooldown=%v", streak, inCooldown)
	}

	// 冷却中不出现在对外列表里
	for _, x := range getFreeModels() {
		if x.ID == "m/dead" {
			t.Fatal("cooled-down model must not be listed")
		}
	}

	// 成功一次即复活
	recordModelSuccess("m/dead")
	modelsMu.Lock()
	m2 := modelsCache["m/dead"]
	revived := !modelInCooldown(m2) && m2.FailStreak == 0
	modelsMu.Unlock()
	if !revived {
		t.Fatalf("success must clear cooldown: %+v", m2)
	}
}

// 配额/鉴权类错误不是模型故障，绝不能因此把模型下架。
func TestModelFailureIgnoresNonModelErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":"rate limit"}`, false},
		{401, `{"error":"unauthorized"}`, false},
		{500, `{"error":"empty response content"}`, false}, // 这是"必须流式"，不是坏模型
		{404, `{"error":"model not found"}`, true},
		{500, `{"error":"stream_initialization_failed"}`, true},
		{500, `{"error":"failed to create stream"}`, true},
		// 上游把"临时限流"也包进 500 inference request failed —— 绝不能因此下架
		{500, `{"error":"inference request failed: temporarily rate-limited upstream, retry shortly"}`, false},
		{500, `{"error":"inference request failed: provider overloaded"}`, false},
		{500, `{"error":"inference request failed: unauthorized"}`, false},
		{500, `{"error":"inference request failed: quota exceeded"}`, false},
		// laguna 的真实形态：免费层结束，藏在 200 的 SSE 错误帧里
		{200, `Failed to create stream: Model 'x-free' not found. If you were using its free tier, that has ended.`, true},
	}
	for _, c := range cases {
		if got := isModelLevelFailure(c.status, c.body); got != c.want {
			t.Fatalf("isModelLevelFailure(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 种子表已删除：没有 live 数据时列表就该是空的，而不是回退到某个会腐烂的快照。
func TestNoSeedModelsOnColdStart(t *testing.T) {
	modelsMu.Lock()
	old := modelsCache
	modelsCache = nil
	modelsMu.Unlock()
	defer func() { modelsMu.Lock(); modelsCache = old; modelsMu.Unlock() }()

	initModelsCache()
	if n := len(getFreeModels()); n != 0 {
		t.Fatalf("cold start must not seed models, got %d", n)
	}
}

// 回归：extraFreeModels 的 id 不在 live feed 里，早期实现每轮同步都会把它们
// 删掉再重建 —— 连带清零自愈累积的 failStreak，导致永远达不到下架阈值。
// （实测：laguna 连续 7 次 500，failStreak 始终是 1。）
func TestPruneKeepsExtraFreeModels(t *testing.T) {
	modelsMu.Lock()
	old := modelsCache
	modelsCache = map[string]*ModelInfo{
		"reflection/beam-free":          {ID: "reflection/beam-free", FailStreak: 2},
		"poolside/laguna-s-2.1-free":    {ID: "poolside/laguna-s-2.1-free", FailStreak: 2},
		"some/dead-model":               {ID: "some/dead-model"},
		"live/ok":                       {ID: "live/ok"},
	}
	live := map[string]bool{"live/ok": true}
	pruned := pruneModelsLocked(live, true)
	_, beamKept := modelsCache["reflection/beam-free"]
	_, lagunaKept := modelsCache["poolside/laguna-s-2.1-free"]
	_, deadStillThere := modelsCache["some/dead-model"]
	streak := modelsCache["reflection/beam-free"].FailStreak
	modelsCache = old
	modelsMu.Unlock()
	_ = streak

	if pruned != 1 || deadStillThere {
		t.Fatalf("only the genuinely dead model should be pruned: pruned=%d deadStillThere=%v", pruned, deadStillThere)
	}
	if !beamKept || !lagunaKept {
		t.Fatal("extraFreeModels must survive pruning (they are catalog gaps, not dead entries)")
	}
	if streak != 2 {
		t.Fatalf("failStreak must survive pruning, got %d", streak)
	}
}

// 回归：绝对不能"上游 200 就记成功"。provider 挂掉时上游先回 200、错误藏在
// SSE 帧里，若在 API 层清零 failStreak，计数会在 1↔2 之间抖动永远达不到阈值
// （实测 laguna 连续 7 次失败，streak 始终 ≤2）。成功的清零只能发生在流
// 真正聚合完成之后。
func TestFailStreakAccumulatesAcrossAttempts(t *testing.T) {
	modelsMu.Lock()
	old := modelsCache
	modelsCache = map[string]*ModelInfo{"m/flaky": {ID: "m/flaky", Status: ModelActive}}
	modelsMu.Unlock()
	defer func() { modelsMu.Lock(); modelsCache = old; modelsMu.Unlock() }()

	body := `Failed to create stream: Model 'x' not found. If you were using its free tier, that has ended.`
	for i := 0; i < modelFailThreshold; i++ {
		// 模拟"API 层成功、流里报错"：只有失败被记录，没有 success 清零
		recordModelFailure("m/flaky", 200, body)
	}
	modelsMu.Lock()
	m := modelsCache["m/flaky"]
	ok := m.FailStreak >= modelFailThreshold && modelInCooldown(m)
	modelsMu.Unlock()
	if !ok {
		t.Fatalf("streak must accumulate to cooldown: %+v", m)
	}
}
