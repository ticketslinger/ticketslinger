package httpapi_test

import "testing"

// Staff mode and suspension are refused for every write by StaffModeWalk and
// SuspensionWalk, which pick regenerateTickets up from the spec.
func TestRegenerateTicketsRoles(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	order, code, token := s.buyLinked(1, "qr@example.com", "evt_regen_roles")
	_, before := s.ticketsOf(code, token)
	for _, c := range []struct{ who, code string }{
		{writeKey, "session_required"},
		{s.member("door@example.com", "checkin"), "forbidden_role"},
		{s.member("nomfa@example.com", "admin-no-mfa"), "mfa_required"},
	} {
		if res := s.do(c.who, "POST", s.orderPath(order)+"/regenerate", `{}`); res.StatusCode != 403 || s.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q, want 403", c.code, res.StatusCode, s.errorCode(res))
		}
	}
	if _, after := s.ticketsOf(code, token); after[0]["qr"] != before[0]["qr"] {
		t.Fatal("a refused caller changed a QR code")
	}
	for _, who := range []string{s.owner, admin} {
		s.must(s.do(who, "POST", s.orderPath(order)+"/regenerate", `{}`), 200)
	}
}

func TestRegenerateTicketsAnswers(t *testing.T) {
	s := newShop(t, nil)
	order, code, token := s.buyLinked(2, "qr@example.com", "evt_regen_answers")
	_, tickets := s.ticketsOf(code, token)
	id, old := tickets[0]["id"].(string), tickets[0]["qr"].(string)
	path := s.orderPath(order) + "/regenerate"

	for _, body := range []string{`{"ticket_ids":[]}`, `{"ticket_ids":null}`} {
		if res := s.do(s.owner, "POST", path, body); res.StatusCode != 422 || s.errorCode(res) != "bad_tickets" {
			t.Errorf("%s: %d %q, want 422 bad_tickets", body, res.StatusCode, s.errorCode(res))
		}
	}
	got := s.must(s.do(s.owner, "POST", path, `{"ticket_ids":["`+id+`"]}`), 200)
	if ids := got["ticket_ids"].([]any); len(ids) != 1 || ids[0] != id || got["email"] != "on_its_way" {
		t.Fatalf("regenerated %v", got)
	}
	_, tickets = s.ticketsOf(code, token)
	if tickets[0]["qr"] == old || tickets[0]["qr"] == nil {
		t.Fatalf("buyer's page still shows the old QR: %v", tickets[0])
	}
	if scan := s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+old+`"}`), 200); scan["result"] != "invalid" {
		t.Fatalf("old QR scans %v, want invalid", scan["result"])
	}
	s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+tickets[0]["qr"].(string)+`"}`), 200)
	if res := s.do(s.owner, "POST", path, `{"ticket_ids":["`+id+`"]}`); res.StatusCode != 409 || s.errorCode(res) != "checked_in" {
		t.Fatalf("checked-in ticket: %d %q, want 409 checked_in", res.StatusCode, s.errorCode(res))
	}
	s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+tickets[1]["qr"].(string)+`"}`), 200)
	if res := s.do(s.owner, "POST", path, `{}`); res.StatusCode != 409 || s.errorCode(res) != "not_regenerable" {
		t.Fatalf("all checked in: %d %q, want 409 not_regenerable", res.StatusCode, s.errorCode(res))
	}
}
