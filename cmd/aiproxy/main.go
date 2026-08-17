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
	"syscall"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/proxy"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "", "path to config.json (default: XDG config dir)")
		addr       = flag.String("addr", "", "listen address (overrides config)")
		headless   = flag.Bool("headless", true, "run without a TUI (the only mode in this build)")
		logLevel   = flag.String("log-level", "info", "debug, info, warn, or error")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("aiproxy", version)
		return
	}

	log := newLogger(*logLevel)
	if err := run(*configPath, *addr, *headless, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func run(configPath, addrOverride string, headless bool, log *slog.Logger) error {
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

	handler, err := buildHandler(cfg, store, log)
	if err != nil {
		return err
	}

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
		log.Info("point your client at it",
			"env", "ANTHROPIC_BASE_URL=http://"+ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// buildHandler wires config into a serving handler. Kept separate from run so
// tests exercise the real composition without binding a port.
func buildHandler(cfg config.Config, store *config.Store, log *slog.Logger) (http.Handler, error) {
	upstreamClient := &http.Client{
		Transport: proxy.NewTransport(proxy.TransportOptions{}),
		Timeout:   60 * time.Second, // control-plane calls only, never the proxy path
	}
	providers := map[string]provider.Provider{
		"anthropic": anthropic.New(upstreamClient),
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
	})

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
		}, log)

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
		OnResult: func(req proxy.Request, res proxy.Result) {
			log.Info("request",
				"model", req.Model, "account", res.AccountID, "status", res.Status,
				"outcome", res.Outcome.String(), "attempts", res.Attempts,
				"ttfbMs", res.TTFBMS, "waitMs", res.WaitMS, "bytes", res.Bytes)
		},
	}), nil
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
