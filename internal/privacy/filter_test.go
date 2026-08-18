package privacy

import (
	"context"
	"strings"
	"testing"
)

func newTestFilter(t *testing.T, dets ...Detector) *Filter {
	t.Helper()
	return New(Options{Detectors: dets, Key: testKey, Unresolved: Passthrough, OnScanFailure: Closed})
}

func TestFilterRedactThenRestoreIsTheIdentity(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)

	body := []byte(`{"messages":[{"role":"user","content":"the SEKRIT value"}]}`)
	redacted, table, err := f.Redact(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "SEKRIT") {
		t.Fatalf("secret survived redaction: %s", redacted)
	}

	// The response echoes the redacted text back; restoring must reproduce the
	// original exactly. This is property 1 through the public surface.
	r := f.Restorer(table)
	out, err := r.Body(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("round trip changed the bytes:\n got %s\nwant %s", out, body)
	}
}

func TestFilterCountsRedactionsByLabel(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)
	if _, _, err := f.Redact(context.Background(),
		[]byte(`{"a":"SEKRIT and SEKRIT","b":"SEKRIT"}`)); err != nil {
		t.Fatal(err)
	}
	snap := f.Snapshot()
	if snap.Redactions["SECRET"] != 1 {
		t.Errorf("Redactions[SECRET] = %d, want 1 — one distinct value however many occurrences",
			snap.Redactions["SECRET"])
	}
}

func TestFilterCountsUnresolvedPlaceholders(t *testing.T) {
	f := newTestFilter(t)
	r := f.Restorer(NewTable(testKey))
	if _, err := r.Body([]byte(`{"a":"orphan [[AIPROXY_SECRET_deadbeef]]"}`)); err != nil {
		t.Fatal(err)
	}
	if got := f.Snapshot().Unresolved; got != 1 {
		t.Errorf("Unresolved = %d, want 1 — this is the one condition that means the agent got something wrong", got)
	}
}

func TestFilterSurfacesAScanError(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "x", err: context.DeadlineExceeded}
	f := newTestFilter(t, det)
	if _, _, err := f.Redact(context.Background(), []byte(`{"a":"a long enough value"}`)); err == nil {
		t.Fatal("Redact hid a detector error; the caller cannot fail closed without it")
	}
}

func TestParseFailureMode(t *testing.T) {
	for in, want := range map[string]FailureMode{"closed": Closed, "open": Open} {
		got, err := ParseFailureMode(in)
		if err != nil || got != want {
			t.Errorf("ParseFailureMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseFailureMode("maybe"); err == nil {
		t.Error("ParseFailureMode accepted an unknown mode; a typo in config must not silently pick one")
	}
}

func TestFilterSnapshotIsSafeUnderConcurrency(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			f.Redact(context.Background(), []byte(`{"a":"SEKRIT here"}`))
		}
	}()
	for i := 0; i < 200; i++ {
		_ = f.Snapshot()
	}
	<-done
}
