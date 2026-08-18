package privacy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// TestFilterPassesAnEmptyBodyThrough covers the shape proxyHandler sees most
// often: it is the router's catch-all, so every GET and every unrecognised path
// arrives with no body at all. WalkStrings errors on empty input, so before this
// guard existed, switching the filter on turned every one of them into a 500
// under the DEFAULT failure mode.
func TestFilterPassesAnEmptyBodyThrough(t *testing.T) {
	f := newTestFilter(t, &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret})
	for _, body := range [][]byte{nil, {}, []byte("   \n\t ")} {
		out, table, err := f.Redact(context.Background(), body)
		if err != nil {
			t.Fatalf("Redact(%q) = %v, want no error", body, err)
		}
		if string(out) != string(body) {
			t.Errorf("Redact(%q) rewrote the body to %q", body, out)
		}
		if table == nil {
			t.Errorf("Redact(%q) returned a nil table; the relay needs a usable one", body)
		} else if table.Len() != 0 {
			t.Errorf("Redact(%q) minted %d placeholders from nothing", body, table.Len())
		}
	}
	if got := f.Snapshot().SentUnfiltered; got != 0 {
		t.Errorf("SentUnfiltered = %d, want 0 — an empty body has nothing to filter", got)
	}
}

// TestFilterPassesANonJSONBodyThroughAndCountsIt pins the documented policy:
// a multipart or form-encoded body is a SHAPE, not a malfunction, so it does not
// fail closed — but it does go upstream unscanned, and property 7 says that must
// never be silent.
func TestFilterPassesANonJSONBodyThroughAndCountsIt(t *testing.T) {
	f := newTestFilter(t, &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret})
	body := []byte("--boundary\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\nSEKRIT\r\n--boundary--")
	out, table, err := f.Redact(context.Background(), body)
	if err != nil {
		t.Fatalf("Redact = %v, want a pass-through rather than a fail-closed refusal", err)
	}
	if string(out) != string(body) {
		t.Errorf("body was rewritten:\n got %q\nwant %q", out, body)
	}
	if table == nil || table.Len() != 0 {
		t.Errorf("want an empty, non-nil table, got %v", table)
	}
	if got := f.Snapshot().SentUnfiltered; got != 1 {
		t.Errorf("SentUnfiltered = %d, want 1 — this body reached upstream unscanned", got)
	}
}

// TestFilterRecordsAScanFailure is property 7 at the counter level: with the
// filter open and a detector that will not run, every request goes upstream
// completely unprotected, and the only trace is here.
func TestFilterRecordsAScanFailure(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           FailureMode
		wantUnfiltered int64
	}{
		{"open sends it anyway", Open, 1},
		{"closed refuses", Closed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New(Options{
				Detectors:     []Detector{&errDetector{}},
				Key:           testKey,
				OnScanFailure: tc.mode,
			})
			if _, _, err := f.Redact(context.Background(), []byte(`{"a":"something long enough"}`)); err == nil {
				t.Fatal("Redact succeeded with a detector that always errors")
			}
			snap := f.Snapshot()
			if snap.LastError == "" {
				t.Error("LastError is empty after a scan failure")
			}
			if snap.SentUnfiltered != tc.wantUnfiltered {
				t.Errorf("SentUnfiltered = %d, want %d", snap.SentUnfiltered, tc.wantUnfiltered)
			}
		})
	}
}

// TestFilterScanTimeoutIsAScanFailure covers the aggregate latency bound: a scan
// that overruns must behave exactly like any other scan failure, so onScanFailure
// governs it rather than the request hanging for as long as the model takes.
func TestFilterScanTimeoutIsAScanFailure(t *testing.T) {
	f := New(Options{
		Detectors:     []Detector{&blockingDetector{}},
		Key:           testKey,
		OnScanFailure: Closed,
		ScanTimeout:   20 * time.Millisecond,
	})
	start := time.Now()
	_, _, err := f.Redact(context.Background(), []byte(`{"a":"something long enough to scan"}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Redact = %v, want a deadline-exceeded scan failure", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Redact took %s; the timeout did not bound it", elapsed)
	}
	if f.Snapshot().LastError == "" {
		t.Error("a timeout left no LastError; it is a scan failure like any other")
	}
}

// errDetector always fails, standing in for a corrupted model install.
type errDetector struct{}

func (errDetector) Name() string { return "err" }
func (errDetector) Scan(context.Context, string) ([]Finding, error) {
	return nil, errors.New("detector is broken")
}

// blockingDetector never returns until its context is done, standing in for the
// model tier's inference time.
type blockingDetector struct{}

func (blockingDetector) Name() string { return "blocking" }
func (blockingDetector) Scan(ctx context.Context, _ string) ([]Finding, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A document too deep to walk is a SCAN FAILURE, not a "not JSON" body.
//
// The two are one line apart in Redactor.Redact and worlds apart in effect:
// ErrNotJSON is passed upstream unfiltered and merely counted, which would hand
// anyone who can nest an array a way to bypass the filter entirely. Fail-closed
// has to refuse it.
func TestFilterTreatsATooDeepBodyAsAScanFailure(t *testing.T) {
	body := []byte(strings.Repeat("[", 8192) + `"x"` + strings.Repeat("]", 8192))

	f := newTestFilter(t)
	out, table, err := f.Redact(context.Background(), body)
	if err == nil {
		t.Fatal("Redact accepted a pathologically nested body")
	}
	if !errors.Is(err, ErrTooDeep) {
		t.Errorf("err = %v, want ErrTooDeep", err)
	}
	if out != nil || table != nil {
		t.Error("a failed scan returned a body and a table; the caller must get nothing to send")
	}
	if snap := f.Snapshot(); snap.SentUnfiltered != 0 {
		t.Errorf("SentUnfiltered = %d under fail-closed; nothing was sent", snap.SentUnfiltered)
	} else if snap.LastError == "" {
		t.Error("LastError is empty; a scan failure must leave a trace")
	}

	// Under fail-open the same body still counts as unfiltered rather than
	// slipping through as an ordinary pass-through shape.
	open := New(Options{Key: testKey, Unresolved: Passthrough, OnScanFailure: Open})
	if _, _, err := open.Redact(context.Background(), body); err == nil {
		t.Fatal("Redact accepted a pathologically nested body under fail-open")
	}
	if n := open.Snapshot().SentUnfiltered; n != 1 {
		t.Errorf("SentUnfiltered = %d, want 1", n)
	}
}
