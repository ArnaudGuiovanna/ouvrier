package provider

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func TestPerMillionRate(t *testing.T) {
	rate := PerMillion(3, 15, 0.30, 3.75)
	if !almostEqual(rate.InputUSDPerToken, 3.0/1_000_000) {
		t.Fatalf("input per token = %v", rate.InputUSDPerToken)
	}
	if !almostEqual(rate.OutputUSDPerToken, 15.0/1_000_000) {
		t.Fatalf("output per token = %v", rate.OutputUSDPerToken)
	}
	if !almostEqual(rate.CacheReadUSDPerToken, 0.30/1_000_000) {
		t.Fatalf("cache read per token = %v", rate.CacheReadUSDPerToken)
	}
	if !almostEqual(rate.CacheWriteUSDPerToken, 3.75/1_000_000) {
		t.Fatalf("cache write per token = %v", rate.CacheWriteUSDPerToken)
	}
}

func TestPricingTableCost(t *testing.T) {
	table := PricingTable{
		"anthropic/claude-sonnet-4-6": PerMillion(3, 15, 0.30, 3.75),
	}
	usage := Usage{InputTokens: 1000, OutputTokens: 500}
	cache := PromptCacheMetadata{ReadInputTokens: 200, WriteInputTokens: 100}
	got, ok := table.Cost("anthropic/claude-sonnet-4-6", usage, cache)
	if !ok {
		t.Fatalf("expected rate found")
	}
	want := 1000*(3.0/1_000_000) +
		500*(15.0/1_000_000) +
		200*(0.30/1_000_000) +
		100*(3.75/1_000_000)
	if !almostEqual(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestPricingTableMissingRate(t *testing.T) {
	table := PricingTable{
		"anthropic/claude-sonnet-4-6": PerMillion(3, 15, 0, 0),
	}
	got, ok := table.Cost("openai/gpt-4o", Usage{InputTokens: 100}, PromptCacheMetadata{})
	if ok {
		t.Fatalf("expected no rate for unknown model")
	}
	if got != 0 {
		t.Fatalf("cost = %v, want 0 for missing rate", got)
	}
}

func TestPricingTableNilOrEmpty(t *testing.T) {
	var nilTable PricingTable
	if _, ok := nilTable.Cost("anthropic/claude-sonnet-4-6", Usage{InputTokens: 100}, PromptCacheMetadata{}); ok {
		t.Fatalf("nil table should not resolve a rate")
	}
	empty := PricingTable{}
	if _, ok := empty.Cost("anthropic/claude-sonnet-4-6", Usage{InputTokens: 100}, PromptCacheMetadata{}); ok {
		t.Fatalf("empty table should not resolve a rate")
	}
}

func TestPricingTableNoCacheTokens(t *testing.T) {
	table := PricingTable{
		"anthropic/claude-sonnet-4-6": PerMillion(3, 15, 0.30, 3.75),
	}
	got, ok := table.Cost("anthropic/claude-sonnet-4-6", Usage{InputTokens: 1000, OutputTokens: 500}, PromptCacheMetadata{})
	if !ok {
		t.Fatalf("expected rate found")
	}
	want := 1000*(3.0/1_000_000) + 500*(15.0/1_000_000)
	if !almostEqual(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}
