package provider

// ModelRate captures USD-per-token rates for a single provider/model.
// Cache rates are optional and default to zero (no cache cost contribution).
type ModelRate struct {
	InputUSDPerToken      float64
	OutputUSDPerToken     float64
	CacheReadUSDPerToken  float64
	CacheWriteUSDPerToken float64
}

// PerMillion builds a ModelRate from USD-per-million-token rates, the most
// common way vendors publish pricing.
func PerMillion(input, output, cacheRead, cacheWrite float64) ModelRate {
	const million = 1_000_000.0
	return ModelRate{
		InputUSDPerToken:      input / million,
		OutputUSDPerToken:     output / million,
		CacheReadUSDPerToken:  cacheRead / million,
		CacheWriteUSDPerToken: cacheWrite / million,
	}
}

// PricingTable maps a provider/model identifier (the same "provider/model"
// form parsed by ParseModelID) to its rate. A nil or empty table resolves no
// rates, leaving cost best-effort (zero) and preserving prior behavior.
type PricingTable map[string]ModelRate

// Cost computes the USD cost for a single call given the model identifier,
// token usage, and prompt cache metadata. The boolean reports whether a rate
// was found; when false the returned cost is zero and callers should leave the
// existing best-effort cost untouched.
//
// Cache read/write tokens are billed against the cache rates when present;
// they are treated as in addition to InputTokens, matching how providers
// report cache tokens separately from billed input tokens.
func (t PricingTable) Cost(model string, usage Usage, cache PromptCacheMetadata) (float64, bool) {
	if len(t) == 0 {
		return 0, false
	}
	rate, ok := t[model]
	if !ok {
		return 0, false
	}
	cost := float64(usage.InputTokens)*rate.InputUSDPerToken +
		float64(usage.OutputTokens)*rate.OutputUSDPerToken +
		float64(cache.ReadInputTokens)*rate.CacheReadUSDPerToken +
		float64(cache.WriteInputTokens)*rate.CacheWriteUSDPerToken
	return cost, true
}
