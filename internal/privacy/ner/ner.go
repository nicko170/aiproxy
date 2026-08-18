package ner

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/onnxrt"
	"github.com/nicko170/aiproxy/internal/privacy/tokenizer"
)

// Options configures the detector.
type Options struct {
	Dir          string   // where Assets were fetched
	Labels       []string // enabled categories; empty means the detector finds nothing
	MaxScanBytes int
	Log          *slog.Logger
}

// window is how many tokens go to the model at once, and overlap is how much two
// consecutive windows share.
//
// The model accepts 128K tokens, but a window that large would put a
// multi-second inference in front of a request. A quarter-window overlap means an
// entity straddling a boundary is seen whole inside at least one window, so
// nothing is missed at the seam; the duplicate findings that produces are removed
// by the (start, end, label) de-duplication below.
const (
	window  = 2048
	overlap = window / 4
)

// Model states reported by ModelState. They are distinct because they call for
// different messages: "absent" means run the install, "installed" means it is
// there and simply has not been needed yet, "error" means the install is broken.
const (
	// stateOff: no category is enabled, so nothing can ever load. Reporting
	// "absent" here would imply something is missing that is not.
	stateOff = "off"
	// stateAbsent: the assets are not on disk (or not at the verified digest).
	// The fix is to fetch them.
	stateAbsent = "absent"
	// stateInstalled: the assets are on disk and verified, but the session has
	// not been built because no scan has needed it yet. Lazy loading is
	// deliberate, so this is the steady state of a correctly installed model on
	// an idle proxy — not a problem to report.
	stateInstalled = "installed"
	stateLoading   = "loading"
	stateReady     = "ready"
	stateError     = "error"
)

// encoder is the tokenizer as the detector uses it.
//
// Deviation from the brief, for the same reason the runner seam exists: with
// the concrete *tokenizer.Tokenizer here, the chunking, offset-mapping,
// truncation, and de-duplication tests would all need the real 27MB
// tokenizer.json and would therefore be skipped in ordinary `go test ./...` —
// which is exactly where the bugs in this file will be. One interface makes
// them run everywhere.
type encoder interface {
	Encode(s string) ([]tokenizer.Token, error)
}

// runner is the only thing Scan needs from ONNX Runtime: given a token window,
// return per-token logits. Keeping it an interface confines the vendored
// binding's API surface to newRunner, so a signature change upstream is a
// one-function fix rather than a rewrite — and lets the chunking, mapping, and
// de-duplication below be tested against a fake.
//
// Run takes a context because the session is process-wide and serialised: one
// request's inference head-of-line blocks every other request that needs the
// model, so a caller whose own deadline has passed must be able to stop waiting
// rather than joining the queue.
type runner interface {
	Run(ctx context.Context, inputIDs, attnMask []int64) ([][]float32, error)
	Close() error
}

// Detector is the model tier. It implements privacy.Detector, so the pipeline
// cannot tell it from the rule table.
//
// The session and the tokenizer are built ONCE, lazily, on the first scan that
// could produce a finding — so a proxy with the model configured but never
// exercised does not dlopen a native library or read 800MB from disk. loadOnce
// carries the outcome, including the failure, so a broken install is reported on
// every scan rather than retried on every scan.
type Detector struct {
	opts     Options
	loadOnce sync.Once
	loadErr  error
	tok      encoder
	labels   []string
	trans    [][]float32
	session  runner
	enabled  map[string]privacy.Label
	state    atomic.Value // string: off, absent, installed, loading, ready, error

	// window and overlap are the constants below, held as fields only so a test
	// can shrink them. Forcing a multi-token entity across a real 2048-token
	// boundary would need megabytes of fixture; at window=8 it is six lines.
	window, overlap int
}

// New builds a detector. It validates every requested category against the
// model's own label set rather than ignoring an unknown one: a typo in config
// must not silently disable protection the operator believes is on.
func New(o Options) (*Detector, error) {
	d := &Detector{opts: o, enabled: make(map[string]privacy.Label, len(o.Labels))}
	for _, name := range o.Labels {
		label, ok := categoryLabels[name]
		if !ok {
			return nil, fmt.Errorf("ner: unknown label %q (known: %v)", name, Categories())
		}
		d.enabled[name] = label
	}
	d.window, d.overlap = window, overlap
	if len(d.enabled) == 0 {
		d.state.Store(stateOff)
		return d, nil
	}
	// Whether the assets are actually on disk is the difference between "run
	// aiproxy privacy install" and "ready, not yet used", and that distinction is
	// surfaced in view.Status.Privacy.ModelState and rendered in the TUI. It can
	// only be answered by verifying digests, which costs a full read of ~850MB —
	// measured at 350-640ms on an SSD, since SHA256 is hardware-accelerated.
	// Paid synchronously and once, on a path that only runs when an operator has
	// opted into the model and already downloaded the weights, in exchange for
	// never reporting a transiently wrong state.
	d.state.Store(stateAbsent)
	if assets, err := Assets(runtime.GOOS, runtime.GOARCH); err == nil {
		if Present(o.Dir, assets) {
			d.state.Store(stateInstalled)
		}
	}
	// An Assets error means this platform has no ONNX Runtime build at all, so
	// nothing can be installed and "absent" is the honest answer; the first scan
	// then fails with the reason.
	return d, nil
}

func (d *Detector) Name() string { return "ner" }

// Scan implements privacy.Detector.
func (d *Detector) Scan(ctx context.Context, text string) ([]privacy.Finding, error) {
	// No enabled labels means no work, and — importantly — no model load. A proxy
	// configured with the model but no labels never dlopens a native library.
	if len(d.enabled) == 0 || len(text) < privacy.MinScanBytes {
		return nil, nil
	}
	if err := d.load(); err != nil {
		return nil, err
	}

	scanned := text
	truncated := false
	if d.opts.MaxScanBytes > 0 && len(scanned) > d.opts.MaxScanBytes {
		// Cut on a rune boundary so the tokenizer is never handed a partial
		// character.
		cut := d.opts.MaxScanBytes
		for cut > 0 && !utf8.RuneStart(scanned[cut]) {
			cut--
		}
		scanned, truncated = scanned[:cut], true
	}
	if truncated && d.opts.Log != nil {
		// Never silent: a truncated scan is a miss, and a miss the operator does
		// not know about is the failure this whole component exists to avoid.
		d.opts.Log.Warn("privacy: input truncated before scanning",
			"bytes", len(text), "scanned", len(scanned), "limit", d.opts.MaxScanBytes)
	}

	toks, err := d.tok.Encode(scanned)
	if err != nil {
		return nil, fmt.Errorf("ner: tokenize: %w", err)
	}
	if len(toks) == 0 {
		return nil, nil
	}

	type key struct {
		start, end int
		label      privacy.Label
	}
	seen := map[key]bool{}
	var out []privacy.Finding
	win, ov := d.window, d.overlap
	if win <= 0 {
		win = window
	}
	if ov < 0 || ov >= win {
		// A step of zero or less would never terminate. Clamping rather than
		// erroring keeps a bad injection a slow scan instead of a hung request.
		ov = win / 4
	}
	for base := 0; base < len(toks); base += win - ov {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(base+win, len(toks))
		chunk := toks[base:end]

		ids := make([]int64, len(chunk))
		mask := make([]int64, len(chunk))
		for i, tk := range chunk {
			ids[i], mask[i] = int64(tk.ID), 1
		}
		logits, err := d.session.Run(ctx, ids, mask)
		if err != nil {
			return nil, fmt.Errorf("ner: inference: %w", err)
		}

		for _, sp := range Decode(logits, d.labels, d.trans) {
			label, enabled := d.enabled[sp.Label]
			if !enabled {
				continue
			}
			if sp.Start < 0 || sp.End > len(chunk) || sp.End <= sp.Start {
				continue // a decode that disagrees with the window is not usable
			}
			// Token indices become byte offsets through the tokenizer's own
			// spans, never by re-decoding and searching — that is where offset
			// bugs come from.
			//
			// min/max across the run rather than first.Start/last.End: token
			// spans do NOT strictly tile. When a character's bytes do not merge
			// into one token the tokenizer gives EVERY resulting token the whole
			// character's span, so consecutive tokens can repeat a span. First
			// and last happen to agree on every boundary observed so far, but
			// that is a monotonicity property nothing states or tests, and
			// min/max costs nothing.
			start, stop := chunk[sp.Start].Start, chunk[sp.Start].End
			for _, tk := range chunk[sp.Start:sp.End] {
				start = min(start, tk.Start)
				stop = max(stop, tk.End)
			}
			// Byte-level BPE attaches the preceding space to a word, so the
			// model's span for "Ada Lovelace" is really " Ada Lovelace" — and
			// redacting that would delete the separator, turning "email Ada"
			// into "email[[AIPROXY_PERSON_...]]". Whitespace is never the
			// sensitive part, so it is given back.
			start, stop = trimSpace(scanned, start, stop)
			if stop <= start {
				continue
			}
			f := privacy.Finding{
				Start: start, End: stop,
				Label: label, Rule: "ner:" + sp.Label, Confidence: sp.Score,
			}
			// De-duplication here is BEST-EFFORT, and deliberately so. It
			// catches the exact duplicate the overlap region produces: an
			// entity seen whole in two windows decodes to the same
			// (start, end, label) twice.
			//
			// What it does NOT catch is the same entity reported with
			// DIFFERENT bounds. An entity straddling a boundary is only
			// partly inside window i, and Decode's "close on O, E, or S"
			// terminal constraint then forces a legal tag onto that window's
			// last token — yielding a TRUNCATED span [p,r). Window i+1 sees
			// the entity whole in the overlap and yields the full [p,q) with
			// q > r. Different keys, so both survive this map and both are
			// returned.
			//
			// That is safe because it is not the last word: privacy.Resolve
			// runs downstream over every detector's findings and drops any
			// span overlapping one already kept, keeping the LONGER of two —
			// so [p,r) is discarded and only [p,q) is redacted. Widening the
			// key or merging spans here would duplicate a guarantee that
			// already exists one layer up, and get it subtly differently.
			// TestBoundaryEntityResolvesToOneFullSpan pins the end-to-end
			// behaviour, because that is where the guarantee actually lives.
			k := key{f.Start, f.End, f.Label}
			if seen[k] {
				continue // the overlap region reported it twice, identically
			}
			seen[k] = true
			out = append(out, f)
		}
		if end == len(toks) {
			break
		}
	}
	return out, nil
}

// trimSpace narrows [start,stop) past leading and trailing ASCII whitespace.
// ASCII only, deliberately: every space a byte-level BPE folds into a token is
// one of these, and walking runes to catch U+00A0 would be work for a case that
// does not arise.
func trimSpace(s string, start, stop int) (int, int) {
	isSpace := func(b byte) bool {
		return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
	}
	for start < stop && isSpace(s[start]) {
		start++
	}
	for stop > start && isSpace(s[stop-1]) {
		stop--
	}
	return start, stop
}

// load builds the tokenizer, labels, transitions, and session exactly once.
//
// The outcome is cached INCLUDING the failure: a broken or absent install is
// reported on every scan rather than retried on every scan, which would turn one
// missing file into a load attempt per request. Every failure wraps
// privacy.ErrModelUnavailable so the control path can answer 503 and name the fix.
func (d *Detector) load() error {
	d.loadOnce.Do(func() {
		d.state.Store(stateLoading)
		defer func() {
			if d.loadErr != nil {
				d.state.Store(stateError)
			} else {
				d.state.Store(stateReady)
			}
		}()

		tok, err := tokenizer.Load(filepath.Join(d.opts.Dir, "tokenizer.json"))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		labels, err := LoadLabels(filepath.Join(d.opts.Dir, "config.json"))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		trans, err := loadTransitions(filepath.Join(d.opts.Dir, "viterbi_calibration.json"), labels)
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		session, err := newRunner(
			filepath.Join(d.opts.Dir, LibraryName(runtime.GOOS)),
			filepath.Join(d.opts.Dir, "model_q4f16.onnx"),
			len(labels))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		d.tok, d.labels, d.trans, d.session = tok, labels, trans, session
	})
	return d.loadErr
}

// ModelState is what view.Status.Privacy reports: off, absent, installed,
// loading, ready, or error.
func (d *Detector) ModelState() string {
	v, _ := d.state.Load().(string)
	if v == "" {
		return stateAbsent
	}
	return v
}

// Close releases the native session. Nothing in the serving path calls it — the
// detector lives as long as the process — but a test that builds several
// detectors would otherwise leak a session and its weights each time.
func (d *Detector) Close() error {
	if d.session == nil {
		return nil
	}
	return d.session.Close()
}

// LibraryName is the ONNX Runtime shared library filename for a platform, and
// must agree with Asset.Name in runtimeAsset — the library is loaded from the
// same directory Ensure wrote it to, not from the system paths, so that an
// unrelated copy on the loader path cannot be picked up instead.
func LibraryName(goos string) string {
	if goos == "darwin" {
		return "libonnxruntime.dylib"
	}
	return "libonnxruntime.so"
}

// ortAPIVersion is the ONNX Runtime C API version the vendored binding
// implements. It must match the library ortVersion pins: the binding overlays a
// Go struct on the C OrtApi struct, so a mismatch reads the wrong function
// pointers.
const ortAPIVersion = 23

// onnxSession is the real runner: the ONLY binding-dependent code in this
// package.
type onnxSession struct {
	rt      *onnxrt.Runtime
	env     *onnxrt.Env
	sess    *onnxrt.Session
	nLabels int

	// sem serialises Run, capacity one. ONNX Runtime documents its sessions as
	// safe for concurrent Run, and its own intra-op pool already parallelises a
	// single inference, but this is the first native library this process loads
	// and a fault inside it is not recoverable in Go. Serialising costs
	// throughput under concurrency and buys a much smaller blast radius.
	//
	// A buffered channel rather than a sync.Mutex because acquisition must be
	// cancellable: this is a process-wide queue, and a request whose scan
	// deadline has expired should not wait in it (see Run).
	sem chan struct{}
}

// newRunner creates the session. This is the ONLY binding-dependent code in the
// package; the signatures it calls are those recorded in
// internal/privacy/onnxrt/VENDOR.md. The model has two inputs, input_ids and
// attention_mask, and one output, logits, of shape [1, seq, len(labels)].
//
// Deviation from the brief's signature: libPath is a parameter because the
// runtime library is fetched into the model directory alongside the weights, and
// dlopening by bare name would search the system paths instead.
func newRunner(libPath, modelPath string, nLabels int) (runner, error) {
	rt, err := onnxrt.NewRuntime(libPath, ortAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", libPath, err)
	}
	env, err := rt.NewEnv("aiproxy-privacy", onnxrt.LoggingLevelError)
	if err != nil {
		_ = rt.Close()
		return nil, err
	}
	sess, err := rt.NewSession(env, modelPath, &onnxrt.SessionOptions{})
	if err != nil {
		env.Close()
		_ = rt.Close()
		return nil, fmt.Errorf("open %s: %w", modelPath, err)
	}
	s := &onnxSession{rt: rt, env: env, sess: sess, nLabels: nLabels, sem: make(chan struct{}, 1)}
	if err := s.checkIO(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// checkIO fails the load rather than the first request when the graph is not the
// two-in/one-out token classifier this package knows how to drive.
func (s *onnxSession) checkIO() error {
	want := map[string]bool{"input_ids": true, "attention_mask": true}
	for _, n := range s.sess.InputNames() {
		delete(want, n)
	}
	if len(want) != 0 {
		return fmt.Errorf("model inputs are %v, want input_ids and attention_mask",
			s.sess.InputNames())
	}
	for _, n := range s.sess.OutputNames() {
		if n == "logits" {
			return nil
		}
	}
	return fmt.Errorf("model outputs are %v, want one named logits", s.sess.OutputNames())
}

func (s *onnxSession) Run(ctx context.Context, inputIDs, attnMask []int64) ([][]float32, error) {
	if len(inputIDs) == 0 || len(inputIDs) != len(attnMask) {
		return nil, fmt.Errorf("ner: %d ids and %d mask entries", len(inputIDs), len(attnMask))
	}
	// A channel rather than a sync.Mutex because sync.Mutex has no cancellable
	// acquire. The session is single-threaded, so every request needing the model
	// queues here; a request whose scan deadline has already expired must leave
	// the queue instead of waiting for inference it will then discard.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.sem }()

	shape := []int64{1, int64(len(inputIDs))}
	idsVal, err := onnxrt.NewTensorValue(s.rt, inputIDs, shape)
	if err != nil {
		return nil, err
	}
	defer idsVal.Close()
	maskVal, err := onnxrt.NewTensorValue(s.rt, attnMask, shape)
	if err != nil {
		return nil, err
	}
	defer maskVal.Close()

	outs, err := s.sess.Run(ctx, map[string]*onnxrt.Value{
		"input_ids":      idsVal,
		"attention_mask": maskVal,
	}, onnxrt.WithOutputNames("logits"))
	// NewTensorValue wraps the Go slices WITHOUT copying them, so they must stay
	// reachable until the native call has read them.
	runtime.KeepAlive(inputIDs)
	runtime.KeepAlive(attnMask)
	if err != nil {
		return nil, err
	}
	for _, v := range outs {
		defer v.Close()
	}

	out, ok := outs["logits"]
	if !ok {
		return nil, fmt.Errorf("ner: model produced no logits output")
	}
	data, dims, err := onnxrt.GetTensorData[float32](out)
	if err != nil {
		return nil, err
	}
	if len(dims) != 3 || dims[0] != 1 || int(dims[1]) != len(inputIDs) || int(dims[2]) != s.nLabels {
		return nil, fmt.Errorf("ner: logits shape %v, want [1 %d %d]", dims, len(inputIDs), s.nLabels)
	}
	rows := make([][]float32, len(inputIDs))
	for i := range rows {
		rows[i] = data[i*s.nLabels : (i+1)*s.nLabels]
	}
	return rows, nil
}

func (s *onnxSession) Close() error {
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
	if s.env != nil {
		s.env.Close()
		s.env = nil
	}
	if s.rt != nil {
		err := s.rt.Close()
		s.rt = nil
		return err
	}
	return nil
}
