package auth

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := CheckPassword(h, "correct horse battery"); !ok {
		t.Fatal("right password rejected")
	}
	if ok, _ := CheckPassword(h, "wrong horse battery"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password here")
	b, _ := HashPassword("same password here")
	if a == b {
		t.Fatal("two hashes of one password must differ")
	}
}

func TestHashUsesPinnedParameters(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash %q, err %v", h, err)
	}
}

func TestArgonConcurrencyIsCapped(t *testing.T) {
	const limit = 2
	oldSem := argonSem
	argonSem = make(chan struct{}, limit)
	var cur, peak atomic.Int32
	argonHook = func(d int) {
		n := cur.Add(int32(d))
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
	}
	t.Cleanup(func() { argonSem, argonHook = oldSem, nil })

	var wg sync.WaitGroup
	for i := 0; i < 4*limit; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = HashPassword("a long passphrase") }()
	}
	wg.Wait()
	if p := peak.Load(); p < 1 || p > limit {
		t.Fatalf("peak concurrent Argon2 runs %d, cap %d", p, limit)
	}
}
