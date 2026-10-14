package testdb

import "testing"

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
