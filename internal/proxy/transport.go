package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// TransportOptions tunes the upstream transport. Zero values take defaults.
type TransportOptions struct {
	MaxConnsPerHost int
	// ResponseHeaderTimeout is a coarse, connection-level backstop, not the
	// per-attempt bound the retry engine enforces (that is retry.headerTimeoutMs,
	// applied by the attempt loop itself; see attempt.go). Zero takes a 120s
	// default here, but that default is arbitrary from the retry engine's point
	// of view and must never be left to silently race retry.headerTimeoutMs: a
	// caller on that path should pass TransportHeaderTimeout(configuredTimeout)
	// instead of the zero value, so this field is always derived from, and kept
	// safely above, the knob an operator actually tunes.
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
}

// NewTransport builds the upstream transport.
//
// HTTP/2 is disabled deliberately, at both the negotiation and the ALPN level.
// One h2 connection multiplexes every request to an origin and shares a single
// flow-control window; an agent client posts large contexts concurrently, so
// those uploads queue behind WINDOW_UPDATE frames and an otherwise trivial
// request can wait minutes for its response headers. Independent HTTP/1.1
// connections have no application-layer flow control, so each upload fills its
// own socket at TCP speed. Re-enabling h2 here will look like a modernization
// and will reintroduce head-of-line blocking under concurrency.
//
// Sockets set NoDelay: Nagle coalescing coalesces small streamed frames and adds
// tens of milliseconds per chunk, which reads to a user as a sluggish stream.
func NewTransport(opts TransportOptions) *http.Transport {
	if opts.MaxConnsPerHost <= 0 {
		opts.MaxConnsPerHost = 256
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 120 * time.Second
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = 90 * time.Second
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
		MaxConnsPerHost:       opts.MaxConnsPerHost,
		MaxIdleConnsPerHost:   opts.MaxConnsPerHost,
		MaxIdleConns:          opts.MaxConnsPerHost * 2,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// HeaderTimeoutMargin is added on top of the configured per-attempt header
// timeout (retry.headerTimeoutMs) when deriving the upstream transport's own
// ResponseHeaderTimeout via TransportHeaderTimeout. The margin exists purely so
// the attempt loop's own timer — which produces the honest, attributable
// server_error outcome named after headerTimeoutMs (see sendWithin in
// attempt.go) — always fires first. Without it, an operator raising
// headerTimeoutMs above whatever this transport happened to be capped at would
// get a generic, unlabelled transport timeout instead, silently disagreeing
// with the knob they just changed.
const HeaderTimeoutMargin = 30 * time.Second

// TransportHeaderTimeout derives the upstream transport's ResponseHeaderTimeout
// from the same value the attempt loop enforces per attempt, so the two can
// never silently disagree. Whatever headerTimeout is configured (including
// values above the 120s this package would otherwise default to), the
// transport's own coarser, backstop-only cutoff is always set safely above it.
//
// headerTimeout <= 0 is treated as "use the attempt loop's own default" so a
// caller that has not resolved its config defaults yet still gets a transport
// timeout consistent with what sendWithin will actually enforce.
func TransportHeaderTimeout(headerTimeout time.Duration) time.Duration {
	if headerTimeout <= 0 {
		headerTimeout = defaultHeaderTimeout
	}
	return headerTimeout + HeaderTimeoutMargin
}
