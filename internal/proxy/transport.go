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
	MaxConnsPerHost       int
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
