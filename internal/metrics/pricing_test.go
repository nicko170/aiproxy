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

// The arithmetic above is verified; the RATES are what drift. A live request
// recorded ~3x its true cost because every Opus row said $15/$75 while the
// published rate is $5/$25, and nothing in this package could tell — every
// other test uses a locally-declared Price. This pins one absolute figure per
// model family against the published card so the table cannot drift silently.
//
// If this test fails, check the published pricing before changing it: a rate
// change is a real event, and the fix is to update the table AND this test
// together, never to relax the assertion.
func TestPricesMatchPublishedRates(t *testing.T) {
	cases := []struct {
		model      string
		input, out float64
	}{
		{"claude-opus-5", 5, 25},
		{"claude-opus-4-8", 5, 25},
		{"claude-opus-4-6-20260101", 5, 25},
		{"claude-sonnet-5", 3, 15}, // introductory $2/$10 through 2026-08-31; see pricing.go
		{"claude-sonnet-4-5-20250929", 3, 15},
		{"claude-haiku-4-5", 1, 5},
		{"claude-fable-5", 10, 50},
		{"claude-mythos-5", 10, 50},
	}
	for _, c := range cases {
		p, ok := PriceFor(c.model)
		if !ok {
			t.Errorf("PriceFor(%q) = not found", c.model)
			continue
		}
		if p.InputPerMTok != c.input {
			t.Errorf("%s input = $%v/MTok, want $%v/MTok", c.model, p.InputPerMTok, c.input)
		}
		if p.OutputPerMTok != c.out {
			t.Errorf("%s output = $%v/MTok, want $%v/MTok", c.model, p.OutputPerMTok, c.out)
		}
	}
}

// The cache columns are derived, not published separately: cache read is a
// tenth of the input rate and cache write is 1.25x it. Pinning the ratio rather
// than the number means a corrected input rate cannot leave a stale cache rate
// behind it.
func TestCacheRatesAreDerivedFromTheInputRate(t *testing.T) {
	for model, p := range prices {
		if got, want := p.CacheReadPerMTok, p.InputPerMTok*0.1; !nearlyEqual(got, want) {
			t.Errorf("%s cache read = %v, want %v (0.1x input)", model, got, want)
		}
		if got, want := p.CacheWritePerMTok, p.InputPerMTok*1.25; !nearlyEqual(got, want) {
			t.Errorf("%s cache write = %v, want %v (1.25x input)", model, got, want)
		}
	}
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// The end-to-end figure a user actually sees. The observed live request that
// exposed the wrong table recorded ~$0.091 for this shape where the truth is
// ~$0.030.
func TestCostForARealisticOpusRequestMatchesThePublishedRate(t *testing.T) {
	got := CostMicros("claude-opus-5", TokenCounts{
		Input: 210, Output: 88, CacheRead: 3000, CacheWrite: 12,
	})
	if got == nil {
		t.Fatal("claude-opus-5 must be priced")
	}
	// 210/1e6*5 + 88/1e6*25 + 3000/1e6*0.50 + 12/1e6*6.25
	//   = 0.00105 + 0.0022 + 0.0015 + 0.000075 = 0.004825 dollars
	const want = 4825
	if *got != want {
		t.Errorf("cost = %d micros, want %d", *got, want)
	}
}
