package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/nicko170/aiproxy/internal/privacy/ner"
	"github.com/nicko170/aiproxy/internal/updater"
)

// Exit codes for the update subcommand. 1 means "an update is available" for
// --check, which is what makes it scriptable; 2 is reserved for errors so a
// wrapper cannot mistake a network failure for good news.
const (
	updateExitOK        = 0
	updateExitAvailable = 1
	updateExitError     = 2
)

// realUpdateClient is the constructor main passes to runUpdate. It is injected
// rather than called directly so a test can point the whole flow at an
// httptest.Server and a temp file without a network or a real install.
func realUpdateClient(current string) *updater.Client {
	return updater.New(updater.DefaultRepo, current)
}

// dispatchSubcommand handles the argument forms that are not "run the proxy".
// It returns -1 when args name no subcommand, meaning main should carry on and
// start the server; any other value is an exit code.
//
// This is a check of the first argument rather than a subcommand framework:
// there is exactly one subcommand, and a framework would be more machinery
// than the feature. An unrecognized first argument is an error rather than a
// silently ignored one — a typo must not boot a proxy.
func dispatchSubcommand(args []string, out io.Writer) int {
	// A leading "-" is a flag, not a subcommand: the server's own flags are
	// parsed by main, and this function must not intercept them.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return -1
	}
	switch args[0] {
	case "update":
		return runUpdate(args[1:], version, out, realUpdateClient)
	case "privacy":
		assets, err := assetsForHost()
		if err != nil {
			fmt.Fprintln(out, "aiproxy:", err)
			return updateExitError
		}
		return runPrivacy(args[1:], out, ner.Dir(), assets)
	default:
		fmt.Fprintf(out, "aiproxy: unknown command %q\nthe subcommands are: update, privacy\nrun aiproxy --help for flags\n", args[0])
		return updateExitError
	}
}

// assetsForHost resolves the privacy filter's model assets for the running
// platform. Assets already reports an unsupported platform as an error
// rather than a panic; this just gives dispatchSubcommand a single call to
// make.
func assetsForHost() ([]ner.Asset, error) {
	return ner.Assets(runtime.GOOS, runtime.GOARCH)
}

// runUpdate implements "aiproxy update" and "aiproxy update --check".
//
// It deliberately does not read the config file. update.checkEnabled governs
// the BACKGROUND check — an outbound request nobody asked for. Running this
// command is the ask, so honouring that setting here would only make an
// explicit instruction fail for a reason the operator did not intend. Not
// touching the config store also means this works before one exists.
func runUpdate(args []string, current string, out io.Writer, newClient func(string) *updater.Client) int {
	fs := flag.NewFlagSet("aiproxy update", flag.ContinueOnError)
	fs.SetOutput(out)
	checkOnly := fs.Bool("check", false, "report whether an update is available without installing it")
	if err := fs.Parse(args); err != nil {
		return updateExitError
	}

	c := newClient(current)
	ctx := context.Background()

	rel, err := c.Check(ctx)
	switch {
	case errors.Is(err, updater.ErrDevBuild):
		fmt.Fprintln(out, "aiproxy: this is a dev build, not a release, so there is nothing to compare against")
		fmt.Fprintln(out, "install a release first: https://github.com/"+updater.DefaultRepo+"/releases")
		return updateExitError
	case errors.Is(err, updater.ErrNoReleases):
		fmt.Fprintln(out, "aiproxy: no releases published yet")
		return updateExitOK
	case err != nil:
		fmt.Fprintln(out, "aiproxy:", err)
		return updateExitError
	}

	if !c.Newer(rel) {
		fmt.Fprintf(out, "aiproxy %s is the latest release\n", current)
		return updateExitOK
	}
	if *checkOnly {
		fmt.Fprintf(out, "aiproxy %s is available (running %s)\n%s\n", rel.Version, current, rel.PageURL)
		return updateExitAvailable
	}

	fmt.Fprintf(out, "downloading aiproxy %s…\n", rel.Version)
	res, err := c.Apply(ctx, rel)
	switch {
	case errors.Is(err, updater.ErrNotWritable):
		fmt.Fprintln(out, "aiproxy:", err)
		fmt.Fprintln(out, "re-run the installer, or update through whichever package manager owns that path:")
		fmt.Fprintln(out, "  curl -fsSL https://raw.githubusercontent.com/"+updater.DefaultRepo+"/main/install.sh | sh")
		return updateExitError
	case errors.Is(err, updater.ErrChecksumMismatch):
		fmt.Fprintln(out, "aiproxy:", err)
		fmt.Fprintln(out, "nothing was changed. try again; if it persists, report it.")
		return updateExitError
	case err != nil:
		fmt.Fprintln(out, "aiproxy:", err)
		return updateExitError
	}

	fmt.Fprintf(out, "updated %s → %s at %s\n", res.PreviousVersion, res.Version, res.Path)
	fmt.Fprintln(out, "restart aiproxy to run the new version")
	return updateExitOK
}
