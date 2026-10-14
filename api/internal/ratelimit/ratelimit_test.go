package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := New(3, time.Minute)
	now := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		if !l.Allow("ip:1", now) {
			t.Fatalf("request %d blocked", i)
		}
	}
	if l.Allow("ip:1", now) {
		t.Fatal("4th request in the window must be blocked")
	}
	if !l.Allow("ip:1", now.Add(time.Minute)) {
		t.Fatal("next window must allow again")
	}
}

func TestAddrKeyGroupsAnIPv6Slash64(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.7":          "203.0.113.7",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"not an address":       "",
	} {
		if got := AddrKey(ip); got != want {
			t.Errorf("AddrKey(%q) = %q, want %q", ip, got, want)
		}
	}
}
