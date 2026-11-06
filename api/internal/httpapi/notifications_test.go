package httpapi_test

import (
	"testing"
)

func TestNotifications(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	door := s.member("door@example.com", "checkin")
	path := "/v1/organizers/acme/notifications"
	get := func(who string) map[string]any { return s.must(s.do(who, "GET", path, ""), 200) }
	if got := get(s.owner); got["order_alerts"] != false {
		t.Fatalf("default %v, want false", got)
	}
	if got := s.must(s.do(s.owner, "PUT", path, `{"order_alerts":true}`), 200); got["order_alerts"] != true {
		t.Fatalf("put %v", got)
	}
	if got := get(s.owner); got["order_alerts"] != true {
		t.Fatalf("after put %v, want true", got)
	}
	if res := s.do(door, "PUT", path, `{"order_alerts":true}`); res.StatusCode != 403 || s.errorCode(res) != "forbidden_role" {
		t.Errorf("checkin put: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(door, "GET", path, ""); res.StatusCode != 403 || s.errorCode(res) != "forbidden_role" {
		t.Errorf("checkin get: %d %q", res.StatusCode, s.errorCode(res))
	}
	for _, m := range []string{"GET", "PUT"} {
		if res := s.do(writeKey, m, path, `{"order_alerts":true}`); res.StatusCode != 403 || s.errorCode(res) != "session_required" {
			t.Errorf("key %s: %d %q", m, res.StatusCode, s.errorCode(res))
		}
	}
	if n := s.count(`select count(*) from memberships where order_alerts`); n != 1 {
		t.Errorf("%d members with alerts on, want 1 (the owner)", n)
	}
}

// Staff mode has no membership row: the answer is false, and a change is refused.
func TestNotificationsInStaffMode(t *testing.T) {
	w := newStaffWorld(t)
	w.must(w.do(w.ownerA, "PUT", "/v1/organizers/acme/notifications", `{"order_alerts":true}`), 200)
	if got := w.must(w.do(w.staffTok, "GET", "/v1/organizers/acme/notifications", ""), 200); got["order_alerts"] != false {
		t.Fatalf("staff GET %v, want false", got)
	}
	if res := w.do(w.staffTok, "PUT", "/v1/organizers/acme/notifications", `{"order_alerts":true}`); res.StatusCode != 403 || w.errorCode(res) != "staff_read_only" {
		t.Errorf("staff PUT: %d %q", res.StatusCode, w.errorCode(res))
	}
}
