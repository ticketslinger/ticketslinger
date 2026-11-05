package testdb

import (
	"testing"
	"time"
)

func TestRequireURLs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	full := map[string]string{"TEST_DATABASE_URL_OWNER": "o", "TEST_DATABASE_URL_APP": "a"}

	if o, a, skip, err := requireURLs(env(full)); o != "o" || a != "a" || skip || err != nil {
		t.Fatalf("set: %q %q %v %v", o, a, skip, err)
	}
	if _, _, skip, err := requireURLs(env(nil)); !skip || err != nil {
		t.Fatalf("local unset: want skip, got skip=%v err=%v", skip, err)
	}
	if _, _, skip, err := requireURLs(env(map[string]string{"CI": "true"})); skip || err == nil {
		t.Fatalf("CI unset: want error, got skip=%v err=%v", skip, err)
	}
}

func TestStale(t *testing.T) {
	cutoff := time.Unix(0x6a000000, 0)
	cases := map[string]bool{
		"t_69ffffff_abcdef012345": true,
		"t_6a000000_abcdef012345": false,
		"t_6a000001_abcdef012345": false,
		"t_abcdef012345":          false, // no timestamp: not ours to judge
		"t_zz_abcdef012345":       false,
		"ticketslinger":           false,
		"ticketslinger_e2e":       false,
	}
	for name, want := range cases {
		if got := stale(name, cutoff); got != want {
			t.Errorf("stale(%q) = %v, want %v", name, got, want)
		}
	}
}
