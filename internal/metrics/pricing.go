package metrics

import "strings"

// Price is a model's rate card in US dollars per million tokens.
type Price struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// TokenCounts is one request's usage.
type TokenCounts struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// prices is a small embedded table keyed by a model-family prefix, so a dated
// variant (claude-sonnet-4-5-20250929) matches its family without needing an
// entry per release. Verify these against current published pricing before
// relying on the figures; they are an estimate, and the schema records NULL
// rather than a guess when a model is absent.
var prices = map[string]Price{
	"claude-opus-5":    {InputPerMTok: 15, OutputPerMTok: 75, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
	"claude-opus-4":    {InputPerMTok: 15, OutputPerMTok: 75, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
	"claude-sonnet-5":  {InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-sonnet-4":  {InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-haiku-4-5": {InputPerMTok: 1, OutputPerMTok: 5, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},
	"claude-fable-5":   {InputPerMTok: 5, OutputPerMTok: 25, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
}

// PriceFor resolves a model name to a rate card, matching the longest known
// prefix so a dated variant resolves to its family.
func PriceFor(model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	best, bestLen, found := Price{}, 0, false
	for prefix, p := range prices {
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			best, bestLen, found = p, len(prefix), true
		}
	}
	return best, found
}

// CostMicros estimates a request's cost in millionths of a dollar, or nil when
// the model has no known price.
//
// nil is deliberate: an unpriced model must record NULL rather than 0, because
// a zero would silently understate a cost total and look like a real answer.
func CostMicros(model string, t TokenCounts) *int64 {
	p, ok := PriceFor(model)
	if !ok {
		return nil
	}
	v := costFromPrice(p, t)
	return &v
}

// costFromPrice charges each token class at its own rate. Cache reads are
// roughly a tenth of the input rate and dominate agent workloads by volume, so
// folding them into the input count would overstate cost by an order of
// magnitude.
func costFromPrice(p Price, t TokenCounts) int64 {
	const perMTok = 1_000_000.0
	dollars := float64(t.Input)/perMTok*p.InputPerMTok +
		float64(t.Output)/perMTok*p.OutputPerMTok +
		float64(t.CacheRead)/perMTok*p.CacheReadPerMTok +
		float64(t.CacheWrite)/perMTok*p.CacheWritePerMTok
	return int64(dollars * 1_000_000)
}
