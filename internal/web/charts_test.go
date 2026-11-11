package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The chart maths are pure functions, so they are worth testing — but node is
// a TEST-TIME convenience only. It is never required to build: `go build`
// produces a working dashboard on a machine with no node at all, which is the
// no-build-step constraint the design turns on.
//
// So this skips loudly rather than failing when node is absent, and CI installs
// node to get the coverage.
func TestChartMath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: skipping the chart-maths tests (they are not a build dependency)")
	}
	// Files, not the directory: node 22+ treats a bare directory argument as
	// a module path rather than a place to discover tests.
	files, err := filepath.Glob("assets/*_test.js")
	if err != nil || len(files) == 0 {
		t.Fatalf("no chart test files found: %v", err)
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed:\n%s", out)
	}
	if !strings.Contains(string(out), "pass") {
		t.Errorf("node --test produced no passing tests:\n%s", out)
	}
}
