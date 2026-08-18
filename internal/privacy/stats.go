package privacy

import "sync"

// Snapshot is the filter's counters at a point in time, for view.Status.
type Snapshot struct {
	// Redactions counts values replaced, per label, PER REQUEST: one increment
	// per distinct value within a single body. A conversation that resends the
	// same secret on every turn therefore counts it on every turn, so this is a
	// measure of how much work the filter did, not of how many distinct secrets
	// exist on this machine.
	Redactions map[string]int64
	// Unresolved counts placeholders that reached a client because the table
	// could not resolve them. It is the one counter that means something went
	// WRONG rather than something worked: a non-zero value means the agent
	// received a placeholder where a real value belonged.
	Unresolved int64
	// SentUnfiltered counts requests that reached upstream with no filtering
	// applied: a scan that failed under the open failure mode, or a body the
	// JSON walker could not read (see Filter.Redact). It is the counter
	// property 7 turns on — "enabled and protecting nothing" is otherwise
	// byte-identical to "enabled and nothing found".
	SentUnfiltered int64
	// LastError is the most recent scan failure, or empty if there has been
	// none. Sticky by design: under the open failure mode nothing else records
	// that the filter stopped working, and a transient error that cleared
	// itself is still something the operator should be able to see.
	LastError string
	// CacheHits and CacheMisses are how the scan cache is doing. A hit rate that
	// collapses is the first symptom of a cache-key bug, and it shows up as a
	// latency problem long before anyone suspects the key.
	CacheHits, CacheMisses int64
}

// stats is the live counter set. One per Filter, shared by every request, so
// every increment is under the mutex — these are updated once per finding, not
// once per byte, so contention is not a concern.
type stats struct {
	mu          sync.Mutex
	redactions  map[string]int64
	unresolved  int64
	unfiltered  int64
	lastError   string
	cacheHits   int64
	cacheMisses int64
}

func newStats() *stats { return &stats{redactions: map[string]int64{}} }

func (s *stats) redacted(label Label) {
	s.mu.Lock()
	s.redactions[string(label)]++
	s.mu.Unlock()
}

func (s *stats) unresolvedSeen() {
	s.mu.Lock()
	s.unresolved++
	s.mu.Unlock()
}

// scanFailed records that a scan could not complete. The message is kept, not
// just a count: "the model failed to load" and "the scan timed out" ask the
// operator for completely different things.
func (s *stats) scanFailed(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastError = err.Error()
	s.mu.Unlock()
}

func (s *stats) sentUnfiltered() {
	s.mu.Lock()
	s.unfiltered++
	s.mu.Unlock()
}

func (s *stats) cache(hit bool) {
	s.mu.Lock()
	if hit {
		s.cacheHits++
	} else {
		s.cacheMisses++
	}
	s.mu.Unlock()
}

func (s *stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{
		Redactions:     make(map[string]int64, len(s.redactions)),
		Unresolved:     s.unresolved,
		SentUnfiltered: s.unfiltered,
		LastError:      s.lastError,
		CacheHits:      s.cacheHits,
		CacheMisses:    s.cacheMisses,
	}
	for k, v := range s.redactions {
		out.Redactions[k] = v
	}
	return out
}
