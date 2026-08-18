// Command aiproxy is a local proxy for AI coding agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/ner"
	"github.com/nicko170/aiproxy/internal/privacy/rules"
	"github.com/nicko170/aiproxy/internal/prober"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/proxy"
	"github.com/nicko170/aiproxy/internal/tui"
	"github.com/nicko170/aiproxy/internal/updater"
	"github.com/nicko170/aiproxy/internal/view"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	// Subcommands are dispatched before flag.Parse so that "aiproxy update
	// --check" reaches the subcommand's own FlagSet: the top-level flag package
	// stops at the first non-flag argument, and --check is not a server flag.
	if code := dispatchSubcommand(os.Args[1:], os.Stdout); code >= 0 {
		os.Exit(code)
	}

	var (
		configPath = flag.String("config", "", "path to config.json (default: XDG config dir)")
		addr       = flag.String("addr", "", "listen address (overrides config)")
		headless   = flag.Bool("headless", false, "run without the TUI, logging to stderr")
		logLevel   = flag.String("log-level", "info", "debug, info, warn, or error")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("aiproxy", version)
		return
	}

	// Under the TUI, slog feeds a ring buffer the Activity screen renders
	// (spec §8): a full-screen program and stderr text cannot share a
	// terminal without corrupting each other.
	var logs *tui.LogRing
	log := newLogger(*logLevel)
	if !*headless {
		logs = tui.NewLogRing(500)
		log = slog.New(logs.Handler(parseLevel(*logLevel)))
	}
	if err := run(*configPath, *addr, *headless, log, logs); err != nil {
		log.Error("fatal", "err", err)
		if !*headless {
			// The ring died with the TUI; say it where it can still be read.
			fmt.Fprintln(os.Stderr, "aiproxy:", err)
		}
		os.Exit(1)
	}
}

func parseLevel(level string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func newLogger(level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func run(configPath, addrOverride string, headless bool, log *slog.Logger, logs *tui.LogRing) error {
	if configPath == "" {
		configPath = config.Path()
	}
	store := config.NewStore(configPath)

	if err := firstRunImport(store, log); err != nil {
		log.Warn("account import skipped", "err", err)
	}

	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if addrOverride != "" {
		cfg.Listen.Addr = addrOverride
	}

	db, err := metrics.Open(config.DBPath())
	if err != nil {
		return fmt.Errorf("open metrics db: %w", err)
	}
	defer db.Close()

	ing := metrics.NewIngester(db, metrics.IngestOptions{Log: log})
	defer ing.Close()

	roller := metrics.NewRoller(db, time.Minute, log)
	roller.Start()
	defer roller.Stop()

	pruner := metrics.NewPruner(db, time.Duration(cfg.Metrics.RetentionDays)*24*time.Hour, log)
	pruner.Start()
	defer pruner.Stop()

	handler, pb, vl, upd, err := buildHandler(cfg, store, log, ing)
	if err != nil {
		return err
	}
	// The background loop is a no-op when quotaProbe.intervalSeconds is 0
	// (see prober.New's doc comment); ProbeNow still works either way, so
	// Start/Stop are unconditional exactly like the roller and pruner above.
	pb.Start()
	defer pb.Stop()

	// Same shape for the update checker: Start always spawns its goroutine so
	// Stop is always safe, and a disabled check simply never reaches the
	// network (see updater.Checker.Start).
	upd.Start()
	defer upd.Stop()

	ln, err := listen(cfg.Listen.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: handler,
		// No global write timeout: a streamed completion legitimately runs for
		// minutes, and a deadline here would sever it mid-answer. Stalls are
		// handled by the relay's idle watchdog instead.
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "accounts", len(cfg.Accounts), "headless", headless)
		log.Info("point Claude Code at it",
			"baseURL", "ANTHROPIC_BASE_URL=http://"+ln.Addr().String(),
			"firstParty", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	shutdown := func() error {
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}

	if headless {
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return shutdown()
		}
	}

	// TUI mode: the program owns the terminal until quit; quitting the TUI
	// shuts the proxy down with it.
	tuiErr := make(chan error, 1)
	go func() {
		tuiErr <- tui.Run(ctx, vl, version, logs)
	}()
	select {
	case err := <-errCh:
		stop() // unwinds the TUI via its context before the terminal is gone
		<-tuiErr
		return err
	case <-ctx.Done():
		<-tuiErr
		return shutdown()
	case err := <-tuiErr:
		if err != nil {
			err = fmt.Errorf("tui: %w (use --headless when not attached to a terminal)", err)
		}
		if serr := shutdown(); serr != nil && err == nil {
			err = serr
		}
		return err
	}
}

// nonEntropyRules drops every rule that leans on an entropy floor to avoid
// false positives, for when the operator has turned entropy-based detection
// off entirely.
//
// Dropping only the floor — keeping the rule active but unqualified — is not
// an option: entropy is what stops the generic "assigned credential" rule
// from firing on every `password = "changeme"` in every example config
// (see rules.Builtin's doc comment). Disabling entropy while leaving that
// rule in place would turn the single most false-positive-prone rule in the
// table into an unconditional one — a false-positive machine, and strictly
// worse than leaving entropy on. So "entropy: false" means "do not use
// entropy-based detection at all," i.e. the rule itself is dropped.
func nonEntropyRules(rs []rules.Rule) []rules.Rule {
	out := make([]rules.Rule, 0, len(rs))
	for _, r := range rs {
		if r.MinEntropy == 0 {
			out = append(out, r)
		}
	}
	return out
}

// buildPrivacy assembles the privacy filter from config, or returns nil when it
// is disabled — which is the default. Detector ORDER is significant: it is the
// tiebreak privacy.Resolve uses for identical spans, so the deterministic rules
// are registered before the model.
func buildPrivacy(cfg config.Config, log *slog.Logger) (*privacy.Filter, error) {
	if !cfg.Privacy.Enabled {
		return nil, nil
	}
	key, err := privacy.LoadOrCreateKey(privacy.KeyPath())
	if err != nil {
		return nil, err
	}
	scanFail, err := privacy.ParseFailureMode(cfg.Privacy.OnScanFailure)
	if err != nil {
		return nil, err
	}
	unresolved := privacy.Passthrough
	if cfg.Privacy.OnUnresolvedPlaceholder == "error" {
		unresolved = privacy.ErrorOut
	}

	// modelState reports the NER model's readiness. It stays nil when no NER
	// category is enabled, which Filter.ModelState reports as "off" — a filter
	// running deterministic rules only is fully functional.
	var modelState func() string

	var dets []privacy.Detector
	if cfg.Privacy.Rules.BuiltinSecrets {
		builtin := rules.Builtin()
		if !cfg.Privacy.Rules.Entropy {
			builtin = nonEntropyRules(builtin)
		}
		rd, err := rules.New(builtin, cfg.Privacy.AllowlistExtra)
		if err != nil {
			return nil, err
		}
		dets = append(dets, rd)
	}
	if len(cfg.Privacy.Denylist) > 0 {
		dl, err := rules.NewDenylist(cfg.Privacy.Denylist)
		if err != nil {
			return nil, err
		}
		dets = append(dets, dl)
	}
	// The model goes LAST, so privacy.Resolve's registration-order tiebreak
	// gives an identical span to a deterministic rule rather than to a
	// probabilistic one.
	if cfg.Privacy.NER.Enabled && len(cfg.Privacy.NER.Labels) > 0 {
		nd, err := ner.New(ner.Options{
			Dir:          ner.Dir(),
			Labels:       cfg.Privacy.NER.Labels,
			MaxScanBytes: cfg.Privacy.NER.MaxScanBytes,
			Log:          log,
		})
		if err != nil {
			return nil, err
		}
		dets = append(dets, nd)
		modelState = nd.ModelState
	}

	// Enabled with nothing to detect is a legitimate config to write by hand and
	// a silent no-op to run: "privacy filter active" is logged, the header shows
	// a filter that is on, Status reports enabled:true, and not one byte is ever
	// examined. Warn rather than refuse — refusing would turn a harmless
	// misconfiguration into a proxy that will not start.
	if len(dets) == 0 {
		log.Warn("privacy filter is enabled but has no detectors; nothing will be scanned. " +
			"Set privacy.rules.builtinSecrets, add privacy.denylist entries, or enable privacy.ner with labels.")
	}

	// The salt carries everything that changes what a scan MEANS, so a toggle or
	// a denylist edit invalidates the cache without any expiry logic.
	salt := privacy.Salt(
		"rules=v1",
		fmt.Sprintf("builtin=%t", cfg.Privacy.Rules.BuiltinSecrets),
		fmt.Sprintf("entropy=%t", cfg.Privacy.Rules.Entropy),
		fmt.Sprintf("deny=%d", len(cfg.Privacy.Denylist)),
		strings.Join(cfg.Privacy.NER.Labels, ","),
	)
	return privacy.New(privacy.Options{
		Detectors:     dets,
		Cache:         privacy.NewCache(cfg.Privacy.CacheEntries, salt),
		Key:           key,
		Unresolved:    unresolved,
		OnScanFailure: scanFail,
		ScanTimeout:   time.Duration(cfg.Privacy.ScanTimeoutMS) * time.Millisecond,
		ModelState:    modelState,
	}), nil
}

// buildHandler wires config into a serving handler. Kept separate from run so
// tests exercise the real composition without binding a port. The returned
// *prober.Prober and *updater.Checker are separate from the handler because
// each owns a background loop with its own lifecycle (Start/Stop), exactly
// like the roller and pruner run constructs alongside them; a caller that
// only wants the handler (most tests) is free to ignore them.
func buildHandler(cfg config.Config, store *config.Store, log *slog.Logger, ing *metrics.Ingester) (http.Handler, *prober.Prober, *view.Local, *updater.Checker, error) {
	upstreamClient := &http.Client{
		Transport: proxy.NewTransport(proxy.TransportOptions{}),
		Timeout:   60 * time.Second, // control-plane calls only, never the proxy path
	}
	anthropicProvider := anthropic.New(upstreamClient)
	providers := map[string]provider.Provider{
		"anthropic": anthropicProvider,
	}

	mgr := account.New(cfg.Accounts, providers, account.Options{
		SwitchThreshold: cfg.Routing.SwitchThreshold,
		SessionAffinity: cfg.Routing.SessionAffinity,
		Ramp:            account.Ramp{Enabled: true},
		Log:             log,
		// A rotated credential is the only way back into an account, so it is
		// written through immediately rather than at shutdown.
		Persist: func(id string, c provider.Credential) error {
			_, err := store.Update(func(cur *config.Config) error {
				for i := range cur.Accounts {
					if cur.Accounts[i].ID == id {
						cur.Accounts[i].Credential = c
						return nil
					}
				}
				return nil
			})
			return err
		},
		OnQuota: func(id string, buckets []provider.QuotaBucket, at int64) {
			for _, b := range buckets {
				ing.RecordQuota(metrics.QuotaSample{
					At: at, AccountID: id, Bucket: b.Name,
					Utilization: b.Utilization, ResetsAt: b.ResetsAt,
				})
			}
		},
	})

	// OnLoginSuccess is the only place a PKCE login's exchanged credential
	// exists after the exchange — set here, after mgr and store both exist,
	// so a successful login persists and goes live without a restart (spec
	// §6.1). See accounts.go's onLoginSuccess for the full "persist, then
	// apply" + re-login-dedupe story.
	anthropicProvider.OnLoginSuccess = onLoginSuccess(store, mgr)

	// The quota prober (spec §6.2): interval 0 disables only its background
	// loop (see prober.New's doc comment), never ProbeNow. Constructed here,
	// alongside mgr and providers, but started/stopped by run() — its
	// lifecycle matches the roller and pruner's, not the handler's.
	pb := prober.New(mgr, providers, time.Duration(cfg.QuotaProbe.IntervalSeconds)*time.Second,
		prober.WithLogger(log))

	// In-app update checking. The Client is told the version this binary was
	// stamped with (see main.version); an unstamped "dev" build is never
	// offered an update, and never makes a request to find one. The Checker's
	// lifecycle matches the prober's above: constructed here, started and
	// stopped by run().
	upd := updater.NewChecker(
		updater.New(updater.DefaultRepo, version),
		cfg.Update.CheckEnabled,
		time.Duration(cfg.Update.CheckIntervalHours)*time.Hour,
		updater.WithCheckerLogger(log),
	)

	// The attempt loop enforces retry.headerTimeoutMs itself (see sendWithin in
	// internal/proxy/attempt.go); the transport's own ResponseHeaderTimeout must
	// be derived from that same value rather than left at its package default, or
	// an operator raising headerTimeoutMs above that default (§6.2 invites this
	// for slower models) would silently hit the transport's coarser cutoff
	// instead, with an error that never mentions headerTimeoutMs at all.
	headerTimeout := time.Duration(cfg.Retry.HeaderTimeoutMS) * time.Millisecond
	attempter := proxy.NewAttempter(mgr, providers,
		proxy.NewTransport(proxy.TransportOptions{
			ResponseHeaderTimeout: proxy.TransportHeaderTimeout(headerTimeout),
		}),
		proxy.RetryConfig{
			Budget:          time.Duration(cfg.Retry.BudgetMS) * time.Millisecond,
			InlineAbsorbMax: time.Duration(cfg.Retry.InlineAbsorbMaxMS) * time.Millisecond,
			BodyIdle:        time.Duration(cfg.Retry.BodyIdleMS) * time.Millisecond,
			HeaderTimeout:   headerTimeout,
			OverloadedBudget: time.Duration(cfg.Retry.OverloadedBudgetMS) *
				time.Millisecond,
		}, log)

	pf, err := buildPrivacy(cfg, log)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("privacy filter: %w", err)
	}
	if pf != nil {
		log.Info("privacy filter active",
			"onScanFailure", cfg.Privacy.OnScanFailure,
			"scanTimeoutMS", cfg.Privacy.ScanTimeoutMS,
			"denylist", len(cfg.Privacy.Denylist),
			"nerLabels", len(cfg.Privacy.NER.Labels))
	}

	// view.Local is the presentation seam (spec §3.1): the control API below
	// reads through it rather than computing anything of its own, which is
	// what lets a future view.HTTP (a detached daemon) replace it without
	// internal/proxy's routes changing at all.
	vl := view.NewLocal(mgr, ing.Store(), store, cfg.Listen.Addr, ing.Dropped, pb, upd, pf)

	return proxy.NewRouter(proxy.HandlerOptions{
		Attempter:     attempter,
		Manager:       mgr,
		APIKey:        cfg.Listen.APIKey,
		BlockedModels: cfg.Routing.BlockedModels,
		Log:           log,
		// Paths bound to the client's own paired identity, relayed with its
		// credential rather than a rotated account's.
		Upstream:            anthropic.DefaultBaseURL,
		PassthroughPrefixes: proxy.DefaultPassthroughPrefixes,
		Dropped:             ing.Dropped,
		View:                vl,
		Privacy:             pf,
		OnResult: func(req proxy.Request, res proxy.Result) {
			log.Info("request",
				"model", req.Model, "account", res.AccountID, "status", res.Status,
				"outcome", res.Outcome.String(), "attempts", res.Attempts,
				"ttfbMs", res.TTFBMS, "waitMs", res.WaitMS, "bytes", res.Bytes,
				"in", res.InputTokens, "out", res.OutputTokens,
				"cacheRead", res.CacheReadTokens, "cacheWrite", res.CacheWriteTokens)

			ing.Record(metrics.Sample{
				StartedAt: res.StartedAt, DurationMS: res.DurationMS,
				TTFBMS: res.TTFBMS, WaitMS: res.WaitMS,
				AccountID: res.AccountID, Provider: "anthropic",
				Model: req.Model, SessionID: req.SessionID,
				Endpoint: endpointOf(req.Path), Status: res.Status,
				Outcome: res.Outcome.String(), Stream: res.Stream,
				Attempts: res.Attempts, Rotated: res.Rotated,
				InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
				CacheReadTokens: res.CacheReadTokens, CacheWriteTokens: res.CacheWriteTokens,
				CostMicros: metrics.CostMicros(req.Model, metrics.TokenCounts{
					Input: res.InputTokens, Output: res.OutputTokens,
					CacheRead: res.CacheReadTokens, CacheWrite: res.CacheWriteTokens,
				}),
			})

			// Same hook, same event: the TUI's activity feed and the dashboard
			// (stage 4/5) will see exactly the requests metrics ingestion sees,
			// which is spec invariant 4 ("one number, one source") extended to
			// the live event stream.
			vl.Publish(view.Event{
				Time: res.StartedAt, Model: req.Model, Account: res.AccountID,
				Status: res.Status, Outcome: res.Outcome.String(), DurationMS: res.DurationMS,
				TTFBMS: res.TTFBMS, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
				CacheReadTokens: res.CacheReadTokens, CacheWriteTokens: res.CacheWriteTokens,
			})
		},
	}), pb, vl, upd, nil
}

// loginLabel matches spec §6.2's persisted account label convention —
// "person@example.com (Org)" — degrading gracefully when a profile is
// missing one half, so a successful login always gets a usable label rather
// than an empty string.
func loginLabel(p provider.Profile) string {
	switch {
	case p.Email != "" && p.OrgName != "":
		return fmt.Sprintf("%s (%s)", p.Email, p.OrgName)
	case p.Email != "":
		return p.Email
	case p.DisplayName != "":
		return p.DisplayName
	default:
		return "logged-in account"
	}
}

// endpointOf strips the query string so /v1/messages?beta=true and
// /v1/messages aggregate together.
func endpointOf(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

// firstRunImport adopts existing credentials so a first run does not require
// re-authorizing every account. It is a first-run action only: with accounts
// already configured it does nothing, so restarts cannot duplicate them.
func firstRunImport(store *config.Store, log *slog.Logger) error {
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if len(cfg.Accounts) > 0 {
		return nil
	}
	legacy := config.LegacyPath()
	if legacy == "" {
		return nil
	}
	imported, err := config.ImportFile(legacy, config.ImportSourceLegacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(imported) == 0 {
		return nil
	}
	if _, err := store.Update(func(c *config.Config) error {
		c.Accounts = append(c.Accounts, imported...)
		return nil
	}); err != nil {
		return err
	}
	log.Info("imported existing accounts", "count", len(imported), "from", legacy)
	return nil
}

// listen binds the client-facing socket, setting NoDelay on each accepted
// connection. Nagle coalescing on small streamed frames adds tens of
// milliseconds per chunk, which reads as a sluggish stream; net.Listener does
// not enable NoDelay by default the way http.Server's own listener does.
func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &noDelayListener{ln}, nil
}

type noDelayListener struct{ net.Listener }

func (l *noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return c, nil
}
