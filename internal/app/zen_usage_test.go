package app

import (
	"testing"
)

// 上游回报的真实计费输入必须覆盖入站估算：日额度按真实值扣，
// 估算漏掉工具 schema 的体积，会让面板严重低估消耗。
func TestApplyZenUsageRecordsRealBilling(t *testing.T) {
	rec := zenStatsRecord{PromptTokens: 4000} // 入站估算（偏小）
	applyZenUsage(&rec, map[string]any{
		"prompt_tokens":     float64(10887),
		"completion_tokens": float64(208),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(8192),
		},
	})
	if rec.UpstreamPromptTokens != 10887 {
		t.Fatalf("UpstreamPromptTokens = %d, want 10887", rec.UpstreamPromptTokens)
	}
	if rec.CachedTokens != 8192 {
		t.Fatalf("CachedTokens = %d, want 8192", rec.CachedTokens)
	}
	if rec.CompletionTokens != 208 {
		t.Fatalf("CompletionTokens = %d, want 208", rec.CompletionTokens)
	}
	if got := billedPromptTokens(&rec); got != 10887 {
		t.Fatalf("billedPromptTokens = %d, want the real 10887 not the estimate", got)
	}
}

// 拿不到 usage 时退回估算（不能记成 0，否则面板会显示"零消耗"）。
func TestBilledTokensFallsBackToEstimate(t *testing.T) {
	rec := zenStatsRecord{PromptTokens: 4200}
	if got := billedPromptTokens(&rec); got != 4200 {
		t.Fatalf("billedPromptTokens = %d, want fallback estimate 4200", got)
	}
}

// usage 缺失子字段时不能 panic 或写脏值。
func TestApplyZenUsageTolerantOfPartialUsage(t *testing.T) {
	rec := zenStatsRecord{}
	applyZenUsage(&rec, map[string]any{"completion_tokens": float64(7)})
	if rec.UpstreamPromptTokens != 0 || rec.CachedTokens != 0 || rec.CompletionTokens != 7 {
		t.Fatalf("unexpected record: %+v", rec)
	}
	applyZenUsage(&rec, map[string]any{})
	applyZenUsage(&rec, nil)
}
