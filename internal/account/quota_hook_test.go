package account

import (
	"sync"
	"testing"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

func TestUpdateQuotaNotifiesTheHook(t *testing.T) {
	var mu sync.Mutex
	var gotID string
	var gotBuckets []provider.QuotaBucket
	var gotAt int64

	m := New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{
			SwitchThreshold: 0.98,
			OnQuota: func(id string, b []provider.QuotaBucket, at int64) {
				mu.Lock()
				defer mu.Unlock()
				gotID, gotBuckets, gotAt = id, b, at
			},
		})

	m.UpdateQuota("a", []provider.QuotaBucket{
		{Name: "5h", Utilization: 0.25, ResetsAt: 1787025600000},
	})

	mu.Lock()
	defer mu.Unlock()
	if gotID != "a" {
		t.Errorf("account id = %q, want a", gotID)
	}
	if len(gotBuckets) != 1 || gotBuckets[0].Name != "5h" || gotBuckets[0].Utilization != 0.25 {
		t.Errorf("buckets = %+v", gotBuckets)
	}
	if gotAt == 0 {
		t.Error("observation timestamp should be set")
	}
}

func TestUpdateQuotaWithNoHookDoesNotPanic(t *testing.T) {
	m := New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{SwitchThreshold: 0.98})

	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.5}})
}

// The hook must not run while the registry lock is held, or a slow observer
// stalls account selection for every in-flight request.
func TestUpdateQuotaHookRunsWithoutHoldingTheLock(t *testing.T) {
	done := make(chan struct{})
	var m *Manager
	m = New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{
			SwitchThreshold: 0.98,
			OnQuota: func(string, []provider.QuotaBucket, int64) {
				// Re-entering the manager from inside the hook deadlocks if the
				// lock is still held.
				m.Snapshot()
				close(done)
			},
		})

	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.5}})
	<-done
}
