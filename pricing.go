package ovr

import "github.com/ArnaudGuiovanna/ouvrier/internal/provider"

// PricingTable maps a "provider/model" identifier to its per-token rate. It is
// used to compute Usage.CostUSD per call and to aggregate total cost per
// execution. A nil or empty table leaves cost best-effort (zero).
type PricingTable = provider.PricingTable

// ModelRate captures USD-per-token rates for a single provider/model. Cache
// rates are optional and default to zero.
type ModelRate = provider.ModelRate

// PerMillion builds a ModelRate from USD-per-million-token rates, the common
// way vendors publish pricing.
func PerMillion(input, output, cacheRead, cacheWrite float64) ModelRate {
	return provider.PerMillion(input, output, cacheRead, cacheWrite)
}
