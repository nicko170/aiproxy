package metrics

import "testing"

func TestCostMicrosIsNilForAnUnknownModel(t *testing.T) {
	if got := CostMicros("some-model-we-have-never-seen", TokenCounts{Input: 1000}); got != nil {
		t.Errorf("cost = %v, want nil — an unpriced model must record NULL, never a plausible wrong number", *got)
	}
}

func TestCostMicrosChargesEachTokenClassSeparately(t *testing.T) {
	// A model priced at $3/MTok in, $15/MTok out, $0.30/MTok cache read,
	// $3.75/MTok cache write.
	p := Price{InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75}
	got := costFromPrice(p, TokenCounts{Input: 1_000_000, Output: 1_000_000,
		CacheRead: 1_000_000, CacheWrite: 1_000_000})

	// 3 + 15 + 0.30 + 3.75 = 22.05 dollars = 22_050_000 micros
	if got != 22_050_000 {
		t.Errorf("cost = %d micros, want 22050000", got)
	}
}

// Cache reads dominate under an agent workload; charging them at the input rate
// would overstate cost by an order of magnitude.
func TestCacheReadIsNotChargedAtTheInputRate(t *testing.T) {
	p := Price{InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75}
	cacheHeavy := costFromPrice(p, TokenCounts{Input: 1000, CacheRead: 1_000_000})
	asIfInput := costFromPrice(p, TokenCounts{Input: 1_001_000})

	if cacheHeavy >= asIfInput {
		t.Errorf("cache-heavy cost %d should be far below the input-rate cost %d", cacheHeavy, asIfInput)
	}
}

func TestPriceForMatchesKnownModelsIncludingDatedVariants(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-4-5-20250929"} {
		if _, ok := PriceFor(model); !ok {
			t.Errorf("PriceFor(%q) = not found; the embedded table should cover current models", model)
		}
	}
}

func TestCostMicrosIsZeroForZeroTokens(t *testing.T) {
	got := CostMicros("claude-opus-5", TokenCounts{})
	if got == nil {
		t.Fatal("a known model with zero tokens should cost 0, not NULL")
	}
	if *got != 0 {
		t.Errorf("cost = %d, want 0", *got)
	}
}
