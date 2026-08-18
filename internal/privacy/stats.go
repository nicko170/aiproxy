package privacy

import "sync"

// Snapshot is the filter's counters at a point in time, for view.Status.
type Snapshot struct {
	// Redactions counts distinct values replaced, per label, since start.
	Redactions map[string]int64
	// Unresolved counts placeholders that reached a client because the table
	// could not resolve them. It is the one counter that means something went
	// WRONG rather than something worked: a non-zero value means the agent
	// received a placeholder where a real value belonged.
	Unresolved int64
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
		Redactions:  make(map[string]int64, len(s.redactions)),
		Unresolved:  s.unresolved,
		CacheHits:   s.cacheHits,
		CacheMisses: s.cacheMisses,
	}
	for k, v := range s.redactions {
		out.Redactions[k] = v
	}
	return out
}
