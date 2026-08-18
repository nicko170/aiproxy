// Package view is the presentation seam (spec §3.1): the one query interface
// both the TUI (stage 4) and the web dashboard (stage 5) read through, so
// neither computes its own aggregates and the two can never disagree.
//
// Every type in this file is a plain, JSON-serializable struct rather than a
// live pointer into proxy, account, or metrics state. That is the seam's
// whole point: a future view.HTTP implementation will marshal these exact
// shapes over the control API, and a value that cannot survive a JSON round
// trip today becomes a defect the moment that implementation exists.
package view

// Status is a snapshot of the running proxy, matching the TUI Overview
// header (spec §8): listen address, uptime, in-flight count, p95 TTFB, and
// any metrics drop count.
type Status struct {
	ListenAddr     string `json:"listenAddr"`
	UptimeSeconds  int64  `json:"uptimeSeconds"`
	InFlight       int    `json:"inFlight"`
	TTFBP95MS      int64  `json:"ttfbP95Ms"`
	MetricsDropped int64  `json:"metricsDropped"`
	// EventsDropped is how many Subscribe events have been discarded for a
	// slow or abandoned subscriber (hub.publish never blocks; see hub.go).
	// Mirrors MetricsDropped's principle: a drop is invisible unless a Status
	// field says so.
	EventsDropped int64 `json:"eventsDropped"`
	// Probe reports the background quota prober's health (spec §6.2): a
	// throttled probe must be visible in the UI, not silently stale.
	Probe ProbeStatus `json:"probe"`
	// Update reports whether a newer release exists, read from the background
	// checker's cache. It is never a live network call: this rides on Status
	// for the same reason Probe does — the TUI's existing status poll renders
	// it for free, with no second poll cycle and no new route — and the check's
	// own cadence is decoupled from that poll entirely.
	Update UpdateStatus `json:"update"`
	// Privacy reports the local privacy filter, following the same precedent as
	// Probe and Update: a fact about the running instance, rendered by the poll
	// the TUI already makes.
	Privacy PrivacyStatus `json:"privacy"`
}

// PrivacyStatus is the privacy filter's state and counters.
type PrivacyStatus struct {
	Enabled bool `json:"enabled"`
	// ModelState is off, absent, downloading, installed, loading, ready, or
	// error. The distinctions carry different messages: "absent" means fetch the
	// assets, "installed" means they are present and verified but no scan has
	// needed the session yet (loading is lazy, so this is the steady state of a
	// correctly installed model on an idle proxy), and "error" means the install
	// is broken.
	ModelState    string `json:"modelState"`
	DownloadedPct int    `json:"downloadedPct,omitempty"`
	// Redactions counts distinct values replaced, per label, this session.
	Redactions map[string]int64 `json:"redactions"`
	// CacheHitRate is 0..1. A collapsed rate is the first symptom of a cache-key
	// bug, which otherwise shows up only as unexplained latency.
	CacheHitRate float64 `json:"cacheHitRate"`
	// Unresolved counts placeholders that reached a client unresolved. It is the
	// one counter here that means something went wrong rather than right.
	Unresolved int64  `json:"unresolved"`
	LastError  string `json:"lastError,omitempty"`
}

// UpdateStatus is what the running instance knows about newer releases.
// Disabled and DevBuild are distinct states from "nothing available": a UI
// that cannot tell them apart ends up saying "up to date" to someone whose
// check is switched off.
type UpdateStatus struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	Available      bool   `json:"available"`
	ReleaseURL     string `json:"releaseUrl,omitempty"`
	// CheckedAt is unix ms of the last completed check, 0 if none.
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// CheckError is the last check's failure. It does not clear LatestVersion
	// or Available: a transient network failure must not make an available
	// update disappear from the UI.
	CheckError string `json:"checkError,omitempty"`
	Disabled   bool   `json:"disabled"`
	DevBuild   bool   `json:"devBuild"`
}

// UpdateResult is what an ApplyUpdate call did.
//
// Updated is false, with a nil error, for the two nothing-to-do outcomes —
// already on the latest release, and no releases published. Those are states,
// not failures, and a UI must be able to render them without reading an error
// string.
type UpdateResult struct {
	Updated         bool   `json:"updated"`
	PreviousVersion string `json:"previousVersion,omitempty"`
	Version         string `json:"version,omitempty"`
	Path            string `json:"path,omitempty"`
	// Message is what the TUI flashes and the CLI prints — the one place
	// "restart to apply" is worded, so both say it identically.
	Message string `json:"message"`
}

// AccountProbeStatus is one account's quota-probe health.
type AccountProbeStatus struct {
	// LastError is the most recent quota-read failure for this account, or
	// "" if the last attempt succeeded (or none has failed yet).
	LastError string `json:"lastError,omitempty"`
	// LastSuccessAt is unix ms of the last successful quota read, or 0 if
	// there has never been one.
	LastSuccessAt int64 `json:"lastSuccessAt,omitempty"`
	// NextAttemptAt is unix ms before which this account is skipped due to
	// exponential backoff after being throttled, or 0 when eligible now.
	NextAttemptAt int64 `json:"nextAttemptAt,omitempty"`
}

// ProbeStatus is the background quota prober's overall health.
type ProbeStatus struct {
	Running         bool                          `json:"running"`
	LastStartedAt   int64                         `json:"lastStartedAt,omitempty"`
	LastCompletedAt int64                         `json:"lastCompletedAt,omitempty"`
	Accounts        map[string]AccountProbeStatus `json:"accounts"`
}

// Account is everything about one account that either front-end may show.
// Deliberately excludes credential material: labels, ids, status, priority
// and quota only, never a token, key, or Authorization value (spec's
// accounting rule extends to the control API surface).
type Account struct {
	ID               string             `json:"id"`
	Label            string             `json:"label"`
	Provider         string             `json:"provider"`
	Priority         int                `json:"priority"`
	Disabled         bool               `json:"disabled"`
	Status           string             `json:"status"`
	LastError        string             `json:"lastError,omitempty"`
	InFlight         int                `json:"inFlight"`
	RateLimitedUntil int64              `json:"rateLimitedUntil,omitempty"`
	PausedUntil      int64              `json:"pausedUntil,omitempty"`
	Buckets          map[string]float64 `json:"buckets"`
}

// Window is a closed-open time range in unix ms, mirroring metrics.Window but
// kept as its own type: nothing outside view.Local is allowed to depend on
// the metrics package's representation.
type Window struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Granularity names a rollup grain a series can be read at.
type Granularity string

const (
	GranularityMinute Granularity = "minute"
	GranularityHour   Granularity = "hour"
)

// GroupBy names the dimension a series is split along.
type GroupBy string

const (
	GroupByAccount GroupBy = "account"
	GroupByModel   GroupBy = "model"
	GroupByOutcome GroupBy = "outcome"
)

// SeriesQuery selects a window, a grain, and a grouping dimension for
// UsageSeries.
type SeriesQuery struct {
	Window      Window      `json:"window"`
	Granularity Granularity `json:"granularity"`
	GroupBy     GroupBy     `json:"groupBy"`
}

// Point is one bucket of one series.
type Point struct {
	BucketStart      int64  `json:"bucketStart"`
	Key              string `json:"key"`
	Requests         int64  `json:"requests"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
	CostMicros       int64  `json:"costMicros"`
}

// Series is the result of UsageSeries.
type Series struct {
	Granularity Granularity `json:"granularity"`
	GroupBy     GroupBy     `json:"groupBy"`
	Points      []Point     `json:"points"`
}

// Totals is an aggregate over a window. UnpricedRequests is how many rows had
// no known price, so a cost total is never presented as complete when it is
// not (spec §7.4).
type Totals struct {
	Requests         int64 `json:"requests"`
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	CostMicros       int64 `json:"costMicros"`
	UnpricedRequests int64 `json:"unpricedRequests"`
}

// Latency is p50/p95 TTFB and duration over a window.
type Latency struct {
	TTFBP50MS     int64 `json:"ttfbP50Ms"`
	TTFBP95MS     int64 `json:"ttfbP95Ms"`
	DurationP50MS int64 `json:"durationP50Ms"`
	DurationP95MS int64 `json:"durationP95Ms"`
}

// QuotaPoint is one observed quota sample for one account.
type QuotaPoint struct {
	At          int64   `json:"at"`
	Bucket      string  `json:"bucket"`
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// Settings is the live-tunable subset of config (spec §6.2): switch
// threshold, retry budget, inline absorb max, header timeout, body idle,
// session affinity, blocked models, quota probe interval, metrics retention,
// and update checking. Everything else in config.Config (listen address,
// accounts, MITM) is not reachable through this call.
type Settings struct {
	SwitchThreshold           float64  `json:"switchThreshold"`
	RetryBudgetMS             int      `json:"retryBudgetMs"`
	InlineAbsorbMaxMS         int      `json:"inlineAbsorbMaxMs"`
	HeaderTimeoutMS           int      `json:"headerTimeoutMs"`
	BodyIdleMS                int      `json:"bodyIdleMs"`
	SessionAffinity           bool     `json:"sessionAffinity"`
	BlockedModels             []string `json:"blockedModels"`
	QuotaProbeIntervalSeconds int      `json:"quotaProbeIntervalSeconds"`
	MetricsRetentionDays      int      `json:"metricsRetentionDays"`
	// UpdateCheckEnabled and UpdateCheckIntervalHours control the background
	// release check. Enabled is live-tunable; the interval is not, because the
	// checker's ticker is built once at startup.
	UpdateCheckEnabled       bool `json:"updateCheckEnabled"`
	UpdateCheckIntervalHours int  `json:"updateCheckIntervalHours"`
	// PrivacyEnabled and the two failure modes configure the local privacy
	// filter. Enabled is restart-gated: the detector set, the cache salt, and the
	// model session are all built once at startup.
	PrivacyEnabled       bool     `json:"privacyEnabled"`
	PrivacyOnScanFailure string   `json:"privacyOnScanFailure"`
	PrivacyOnUnresolved  string   `json:"privacyOnUnresolved"`
	PrivacyDenylist      []string `json:"privacyDenylist"`
}

// Applied reports which fields an UpdateSettings call actually put into
// effect on the running proxy versus which were persisted but require a
// restart to take effect (see UpdateSettings's doc comment for why some
// fields are restart-gated).
//
// This is returned as data, deliberately, rather than left to a doc comment:
// a stage-4 settings screen decodes a JSON response and cannot show a
// pending field as already applied without actively ignoring a value it
// already parsed. A comment can only be read by a person, not by the code
// that renders the result. When a currently restart-gated field later
// becomes live, only the classification here narrows — no signature or
// wire-format change is needed for callers already handling both lists.
type Applied struct {
	Live         []string `json:"live"`
	NeedsRestart []string `json:"needsRestart"`
}

// Event is one completed request, published to every Subscribe-r. It carries
// only the fields spec §9's Activity screen needs: time, model, account
// label, status, outcome, duration, TTFB, and token counts — never a
// credential.
type Event struct {
	Time             int64  `json:"time"`
	Model            string `json:"model"`
	Account          string `json:"account"`
	Status           int    `json:"status"`
	Outcome          string `json:"outcome"`
	DurationMS       int64  `json:"durationMs"`
	TTFBMS           int64  `json:"ttfbMs"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
}
