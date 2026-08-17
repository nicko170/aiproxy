package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func TestNewTransportDisablesHTTP2(t *testing.T) {
	tr := NewTransport(TransportOptions{})

	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be false")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("NextProtos = %v, want [http/1.1]", got)
	}
}

// The behavioural assertion, not just the config one: against a server that
// offers HTTP/2, this transport must still negotiate HTTP/1.1.
//
// A single h2 connection multiplexes every request to an origin behind one
// flow-control window. Agent clients post large contexts concurrently, so those
// uploads queue behind WINDOW_UPDATE frames and a trivial request can wait
// minutes for headers. Independent h1 connections each fill their own socket.
func TestTransportNegotiatesHTTP1AgainstAnHTTP2Server(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := NewTransport(TransportOptions{})
	tr.TLSClientConfig.InsecureSkipVerify = true // self-signed test cert
	client := &http.Client{Transport: tr}

	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.Proto != "HTTP/1.1" {
		t.Errorf("client saw %s, want HTTP/1.1", res.Proto)
	}
	if string(body) != "HTTP/1.1" {
		t.Errorf("server saw %s, want HTTP/1.1", body)
	}
}

func TestNewTransportAppliesOptionsAndDefaults(t *testing.T) {
	tr := NewTransport(TransportOptions{
		MaxConnsPerHost:       7,
		ResponseHeaderTimeout: 3 * time.Second,
	})
	if tr.MaxConnsPerHost != 7 {
		t.Errorf("MaxConnsPerHost = %d, want 7", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 3*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}

	def := NewTransport(TransportOptions{})
	if def.MaxConnsPerHost != 256 {
		t.Errorf("default MaxConnsPerHost = %d, want 256", def.MaxConnsPerHost)
	}
	if def.MaxIdleConnsPerHost != 256 {
		t.Errorf("default MaxIdleConnsPerHost = %d, want 256", def.MaxIdleConnsPerHost)
	}
}

// Observes the socket option itself rather than merely asserting a hook was
// installed: the transport's own DialContext is used to dial a real listener and
// TCP_NODELAY is read back off the resulting file descriptor.
//
// Nagle coalescing on small streamed frames adds tens of milliseconds per chunk,
// which reads to a user as a sluggish stream.
func TestTransportDialedSocketsHaveNoDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	tr := NewTransport(TransportOptions{})
	if tr.DialContext == nil {
		t.Fatal("NewTransport must install a DialContext that can set NoDelay")
	}

	conn, err := tr.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	tc, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialed conn is %T, want *net.TCPConn", conn)
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var noDelay int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		noDelay, optErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if optErr != nil {
		t.Fatalf("getsockopt TCP_NODELAY: %v", optErr)
	}
	if noDelay == 0 {
		t.Error("TCP_NODELAY is off on a dialed socket; Nagle will coalesce streamed frames")
	}
}
