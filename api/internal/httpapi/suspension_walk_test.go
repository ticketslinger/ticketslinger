package httpapi_test

import (
	"strings"
	"testing"
)

// Test 23: every organizer operation on a suspended organizer, as its owner,
// a write key and a read key. Reads answer as they did before; everything
// else is refused, and never with a 2xx.
func TestSuspensionWalk(t *testing.T) {
	w := newStaffWorld(t)
	_, writeKey := w.createKey(w.ownerA, "acme", "write")
	_, readKey := w.createKey(w.ownerA, "acme", "read")
	callers := map[string]string{"owner": w.ownerA, "write key": writeKey, "read key": readKey}

	type call struct{ id, method, url string }
	var ops []call
	eachOp(t, w.fixture, func(id, method, path, url string) {
		if strings.HasPrefix(path, "/organizers/{organizer}") {
			ops = append(ops, call{id, method, "/v1" + url})
		}
	})
	if len(ops) < 40 {
		t.Fatalf("walked %d organizer operations; the walk is broken", len(ops))
	}
	before := map[string]int{}
	for _, op := range ops {
		if op.method == "GET" {
			for name, tok := range callers {
				before[name+" "+op.url] = w.do(tok, op.method, op.url, "").StatusCode
			}
		}
	}

	if b := w.act(w.staffTok, "suspend"); b["status"] != 200 {
		t.Fatalf("suspend: %v", b)
	}

	for _, op := range ops {
		for name, tok := range callers {
			res := w.do(tok, op.method, op.url, `{}`)
			got := w.errorCode(res)
			if op.method == "GET" {
				if want := before[name+" "+op.url]; res.StatusCode != want {
					t.Errorf("%s: GET %s (%s) got %d %q, want %d as before", name, op.url, op.id, res.StatusCode, got, want)
				}
				continue
			}
			ok := res.StatusCode == 403 && got == "organizer_suspended"
			if _, sess := sessionOnly[op.id]; sess && name != "owner" {
				ok = res.StatusCode == 403 && got == "session_required"
			} else if name == "read key" {
				ok = ok || res.StatusCode == 403 && got == "scope_read_only"
			}
			if !ok {
				t.Errorf("%s: %s %s (%s) got %d %q, want 403 organizer_suspended", name, op.method, op.url, op.id, res.StatusCode, got)
			}
		}
	}
}
