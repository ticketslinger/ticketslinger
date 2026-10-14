package auth

import "testing"

// LowerCostForTests drops Argon2id to m=1 MiB, t=1, p=1 so test suites that
// hash passwords finish in seconds. It panics outside a test binary, so a
// production build cannot run at the lowered cost. CheckPassword reads the
// cost from each stored hash, so hashes made either way still verify.
func LowerCostForTests() {
	if !testing.Testing() {
		panic("auth: LowerCostForTests called outside a test binary")
	}
	argonCost = argonParams{memory: 1024, time: 1, threads: 1}
}
