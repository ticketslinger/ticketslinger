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
