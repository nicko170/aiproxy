package main

import (
	"context"
	"fmt"
	"io"

	"github.com/nicko170/aiproxy/internal/privacy/ner"
)

// runPrivacy implements "aiproxy privacy install" and "aiproxy privacy
// status". Both operate on the directory and asset list the caller supplies
// rather than reaching for ner.Dir or the host's runtime.GOOS/GOARCH
// themselves, so a test can point them at an httptest.Server and a temp
// directory without a network or a real install.
//
// Exit codes follow the update subcommand's convention: 0 on success, 2 on
// error. There is no "1" here — unlike "update --check", neither action is a
// machine-readable yes/no check; "status" reports 2 when assets are missing
// simply because that is the error case a script would want to detect.
func runPrivacy(args []string, out io.Writer, dir string, assets []ner.Asset) int {
	if len(args) != 1 {
		return privacyUsage(out)
	}
	switch args[0] {
	case "install":
		return runPrivacyInstall(out, dir, assets)
	case "status":
		return runPrivacyStatus(out, dir, assets)
	default:
		return privacyUsage(out)
	}
}

func privacyUsage(out io.Writer) int {
	fmt.Fprintln(out, "usage: aiproxy privacy install | status")
	fmt.Fprintln(out, "  install   download and verify the local NER model")
	fmt.Fprintln(out, "  status    report whether the model is installed")
	return updateExitError
}

// runPrivacyInstall fetches whatever of assets is missing or fails
// verification. The total size is printed before anything is downloaded —
// this is roughly 850MB, not something to start silently — and progress is
// reported per asset as Ensure downloads it.
func runPrivacyInstall(out io.Writer, dir string, assets []ner.Asset) int {
	var total int64
	for _, a := range assets {
		total += a.Bytes
	}
	fmt.Fprintf(out, "aiproxy: downloading the privacy filter's model (%s) to %s\n", humanBytes(total), dir)

	lastPct := make(map[string]int64, len(assets))
	progress := func(name string, done, total int64) {
		if total <= 0 {
			return
		}
		pct := done * 100 / total
		// Ensure calls back on every chunk; only print when the percentage
		// actually moves, or a fast asset would flood the terminal with
		// hundreds of identical lines.
		if p, ok := lastPct[name]; ok && p == pct {
			return
		}
		lastPct[name] = pct
		fmt.Fprintf(out, "  %s: %d%%\n", name, pct)
	}
	if err := ner.Ensure(context.Background(), dir, assets, progress); err != nil {
		fmt.Fprintln(out, "aiproxy: privacy install:", err)
		return updateExitError
	}
	fmt.Fprintf(out, "aiproxy: installed to %s\n", dir)
	return updateExitOK
}

// runPrivacyStatus reports each asset's name and whether it is present on
// disk at the verified digest, exiting 0 only when every one is.
func runPrivacyStatus(out io.Writer, dir string, assets []ner.Asset) int {
	allPresent := true
	for _, a := range assets {
		present := ner.Present(dir, []ner.Asset{a})
		status := "present, verified"
		if !present {
			status = "missing"
			allPresent = false
		}
		fmt.Fprintf(out, "  %-24s %s\n", a.Name, status)
	}
	if !allPresent {
		fmt.Fprintln(out, "aiproxy: some assets are missing; run `aiproxy privacy install` to fetch them")
		return updateExitError
	}
	fmt.Fprintf(out, "aiproxy: all assets present and verified in %s\n", dir)
	return updateExitOK
}

func humanBytes(n int64) string {
	const mb = 1 << 20
	return fmt.Sprintf("~%.0f MB", float64(n)/mb)
}
