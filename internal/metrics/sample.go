package metrics

// Sample is one completed request, ready to persist. Plain data: the metrics
// package depends on neither the proxy nor the account registry.
type Sample struct {
	StartedAt  int64 // unix ms
	DurationMS int64
	TTFBMS     int64
	WaitMS     int64

	AccountID     string
	Provider      string
	Model         string
	UpstreamModel string
	SessionID     string
	Endpoint      string

	Status   int
	Outcome  string
	Stream   bool
	Attempts int
	Rotated  bool

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	// CostMicros is nil when the model has no known price, so an unpriced model
	// records NULL rather than a plausible wrong number.
	CostMicros *int64
}

// QuotaSample is one observation of an account's quota window.
type QuotaSample struct {
	At          int64 // unix ms
	AccountID   string
	Bucket      string
	Utilization float64
	ResetsAt    int64
}
