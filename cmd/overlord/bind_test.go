package main

import (
	"testing"
)

func TestResolveBindAddr(t *testing.T) {
	cases := []struct {
		name     string
		bindFlag string
		portFlag string
		envBind  string
		wantAddr string
		wantErr  bool
	}{
		{"defaults (no flags, no env)", "", "8080", "", "127.0.0.1:8080", false},
		{"port only", "", "9000", "", "127.0.0.1:9000", false},
		{"bind host only, default port", "0.0.0.0", "8080", "", "0.0.0.0:8080", false},
		{"bind host:port overrides port flag", "10.0.0.5:7777", "8080", "", "10.0.0.5:7777", false},
		{"bind host-only + non-default port", "127.0.0.1", "9090", "", "127.0.0.1:9090", false},
		{"env OVERLORD_BIND supplies host", "", "8080", "10.0.0.5", "10.0.0.5:8080", false},
		{"flag beats env", "127.0.0.1", "8080", "10.0.0.5", "127.0.0.1:8080", false},
		{"IPv6 literal", "[::1]:8080", "9090", "", "[::1]:8080", false},
		{"empty port rejected", "127.0.0.1", "", "", "", true},
		{"invalid bind host rejected", "not a host", "8080", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBindAddr(tc.bindFlag, tc.portFlag, tc.envBind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveBindAddr(%q,%q,%q) err=%v wantErr=%v", tc.bindFlag, tc.portFlag, tc.envBind, err, tc.wantErr)
			}
			if got != tc.wantAddr {
				t.Errorf("resolveBindAddr(%q,%q,%q) = %q, want %q", tc.bindFlag, tc.portFlag, tc.envBind, got, tc.wantAddr)
			}
		})
	}
}
