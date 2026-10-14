package idempotency

import (
	"strings"
	"testing"
)

func TestValidKey(t *testing.T) {
	for k, want := range map[string]bool{
		"": false, "a": true, "a b~": true, strings.Repeat("a", 255): true, strings.Repeat("a", 256): false,
		"a\x01b": false, "a\x7fb": false, "caf\u00e9": false,
	} {
		if validKey(k) != want {
			t.Errorf("validKey(%q) = %v, want %v", k, !want, want)
		}
	}
}
