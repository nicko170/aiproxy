package main

import (
	"strings"
	"testing"
)

func TestBindGuard(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		apiKey   string
		insecure bool
		wantErr  bool
	}{
		{"loopback without a key is fine", "127.0.0.1:3456", "", false, false},
		{"loopback with a key is fine", "127.0.0.1:3456", "k", false, false},
		{"localhost name resolves as loopback", "localhost:3456", "", false, false},
		{"ipv6 loopback is fine", "[::1]:3456", "", false, false},
		{"public bind with a key is fine", "0.0.0.0:3456", "k", false, false},
		{"public bind without a key is refused", "0.0.0.0:3456", "", false, true},
		{"specific public address without a key is refused", "203.0.113.9:3456", "", false, true},
		{"empty host means all interfaces and is refused", ":3456", "", false, true},
		{"the escape hatch permits it", "0.0.0.0:3456", "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkBindSafety(c.addr, c.apiKey, c.insecure)
			if c.wantErr && err == nil {
				t.Fatal("expected a refusal, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if c.wantErr && !strings.Contains(err.Error(), "apiKey") {
				t.Errorf("refusal does not name the fix: %v", err)
			}
		})
	}
}
