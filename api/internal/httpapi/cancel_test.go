package httpapi_test

import "testing"

// Staff mode and suspension are refused for every write by StaffModeWalk and
// SuspensionWalk, which pick cancelOrder up from the spec.
func TestCancelOrderRoles(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	for _, c := range []struct {
		who, code string
		status    int
	}{
		{writeKey, "session_required", 403},
		{s.member("door@example.com", "checkin"), "forbidden_role", 403},
		{s.member("nomfa@example.com", "admin-no-mfa"), "mfa_required", 403},
	} {
		order := s.buyPaid(1, "paid@example.com", "evt_cancel_roles_"+c.code)
		res := s.do(c.who, "POST", s.orderPath(order)+"/cancel", `{}`)
		if res.StatusCode != c.status || s.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q", c.code, res.StatusCode, s.errorCode(res))
		}
		if n := s.count(`select count(*) from tickets where order_id = $1 and status = 'void'`, order); n != 0 {
			t.Errorf("%s: a refused caller cancelled a ticket", c.code)
		}
	}
	for i, who := range []string{s.owner, admin} {
		order := s.buyPaid(1, "paid@example.com", "evt_cancel_ok_"+string(rune('a'+i)))
		s.must(s.do(who, "POST", s.orderPath(order)+"/cancel", `{"notify":false}`), 200)
	}
}

func TestCancelOrderAnswers(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(2, "paid@example.com", "evt_cancel_answers")
	for _, body := range []string{`{"ticket_ids":[]}`, `{"ticket_ids":null}`} {
		res := s.do(s.owner, "POST", s.orderPath(order)+"/cancel", body)
		if res.StatusCode != 422 || s.errorCode(res) != "bad_tickets" {
			t.Errorf("%s: %d %q, want 422 bad_tickets", body, res.StatusCode, s.errorCode(res))
		}
	}
	got := s.must(s.do(s.owner, "POST", s.orderPath(order)+"/cancel", `{"reason":"Double booked"}`), 200)
	if got["order_status"] != "cancelled" || got["charge_cents"] != float64(5000) || len(got["ticket_ids"].([]any)) != 2 {
		t.Fatalf("cancelled %v", got)
	}
	if n := s.count(`select count(*) from jobs where kind = 'send_email' and payload->>'subject' = 'Tickets cancelled for Gala'`); n != 1 {
		t.Fatalf("%d cancellation emails queued, want 1 (notify defaults to true)", n)
	}
	if res := s.do(s.owner, "POST", s.orderPath(order)+"/cancel", `{}`); res.StatusCode != 409 || s.errorCode(res) != "not_cancellable" {
		t.Fatalf("again: %d %q, want 409 not_cancellable", res.StatusCode, s.errorCode(res))
	}
}
