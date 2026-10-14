package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}
	cases := []struct{ name, remote, realIP, want string }{
		{"trusted proxy with header", "127.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"trusted v6 proxy with header", "[::1]:5000", "2001:db8::1", "2001:db8::1"},
		{"untrusted peer with header", "198.51.100.7:5000", "203.0.113.9", "198.51.100.7"},
		{"trusted proxy, invalid header", "127.0.0.1:5000", "not-an-ip", "127.0.0.1"},
		{"trusted proxy, no header", "127.0.0.1:5000", "", "127.0.0.1"},
		{"zone dropped", "[fe80::1%en0]:5000", "", "fe80::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		r.Header.Set("X-Forwarded-For", "192.0.2.1")
		if got := clientIP(r, trusted); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
