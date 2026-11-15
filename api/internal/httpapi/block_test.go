package httpapi_test

import (
	"strings"
	"testing"
)

// Staff mode and suspension are refused for every write by StaffModeWalk and
// SuspensionWalk, which pick blockTicket and unblockTicket up from the spec.
func TestBlockTicketRoles(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	order, code, token := s.buyLinked(1, "hold@example.com", "evt_block_roles")
	_, tickets := s.ticketsOf(code, token)
	path := s.orderPath(order) + "/tickets/" + tickets[0]["id"].(string) + "/block"
	for _, c := range []struct {
		who, code string
	}{
		{writeKey, "session_required"},
		{s.member("door@example.com", "checkin"), "forbidden_role"},
		{s.member("nomfa@example.com", "admin-no-mfa"), "mfa_required"},
	} {
		for _, method := range []string{"PUT", "DELETE"} {
			if res := s.do(c.who, method, path, `{}`); res.StatusCode != 403 || s.errorCode(res) != c.code {
				t.Errorf("%s %s: got %d %q, want 403", method, c.code, res.StatusCode, s.errorCode(res))
			}
		}
	}
	if n := s.count(`select count(*) from tickets where order_id = $1 and blocked_at is not null`, order); n != 0 {
		t.Fatal("a refused caller blocked a ticket")
	}
	for _, who := range []string{s.owner, admin} {
		s.must(s.do(who, "PUT", path, `{"reason":"Chargeback"}`), 204)
		s.must(s.do(who, "DELETE", path, ""), 204)
	}
}

func TestBlockTicketAnswers(t *testing.T) {
	s := newShop(t, nil)
	order, code, token := s.buyLinked(2, "hold@example.com", "evt_block_answers")
	other, otherCode, otherToken := s.buyLinked(1, "other@example.com", "evt_block_other")
	_, tickets := s.ticketsOf(code, token)
	_, theirs := s.ticketsOf(otherCode, otherToken)
	id := tickets[0]["id"].(string)
	path := s.orderPath(order) + "/tickets/" + id + "/block"
	qr := tickets[0]["qr"].(string)

	s.must(s.do(s.owner, "PUT", path, `{"reason":"Chargeback on the card"}`), 204)
	_, tickets = s.ticketsOf(code, token)
	if tickets[0]["blocked"] != true || tickets[0]["qr"] != nil || tickets[0]["qr_png"] != nil || tickets[1]["blocked"] != false || tickets[1]["qr"] == nil {
		t.Fatalf("buyer's page after a block: %v", tickets)
	}
	d := s.must(s.do(s.owner, "GET", s.orderPath(order), ""), 200)
	dt := d["tickets"].([]any)[0].(map[string]any)
	if dt["blocked_at"] == nil || dt["blocked_reason"] != "Chargeback on the card" || dt["checked_in_at"] != nil || dt["ticket_type_id"] != s.f["ticketType"] {
		t.Fatalf("order detail ticket: %v", dt)
	}
	scan := s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+qr+`"}`), 200)
	if scan["result"] != "blocked" || scan["ticket"].(map[string]any)["blocked"] != true {
		t.Fatalf("scan of a blocked ticket: %v", scan)
	}
	s.must(s.do(s.owner, "PUT", path, `{}`), 204) // already blocked: nothing changes

	res := s.do(s.owner, "PUT", path, `{"reason":"`+strings.Repeat("a", 201)+`"}`)
	if body := readBody(res); res.StatusCode != 422 || !strings.Contains(body, `"bad_reason"`) || !strings.Contains(body, `"reason"`) {
		t.Fatalf("201-character reason: %d %s, want 422 bad_reason", res.StatusCode, body)
	}
	if res := s.do(s.owner, "PUT", s.orderPath(order)+"/tickets/"+theirs[0]["id"].(string)+"/block", `{}`); res.StatusCode != 404 {
		t.Fatalf("another order's ticket: %d, want 404", res.StatusCode)
	}
	if res := s.do(s.owner, "DELETE", s.orderPath(other)+"/tickets/"+id+"/block", ""); res.StatusCode != 404 {
		t.Fatalf("this ticket under another order: %d, want 404", res.StatusCode)
	}

	s.must(s.do(s.owner, "DELETE", path, ""), 204)
	s.must(s.do(s.owner, "DELETE", path, ""), 204) // not blocked: nothing changes
	if _, tickets = s.ticketsOf(code, token); tickets[0]["blocked"] != false || tickets[0]["qr"] != qr {
		t.Fatalf("buyer's page after unblocking: %v", tickets[0])
	}
	if n := s.count(`select count(*) from audit_log where entity_id = $1 and action in ('ticket.blocked', 'ticket.unblocked')`, id); n != 2 {
		t.Fatalf("%d block audit rows, want 2", n)
	}
	if n := s.count(`select count(*) from audit_log where strpos(audit_log::text, $1) > 0`, qr); n != 0 {
		t.Fatal("an audit row carries the ticket's QR")
	}

	s.must(s.do(s.owner, "POST", s.orderPath(order)+"/cancel", `{"notify":false,"ticket_ids":["`+id+`"]}`), 200)
	for _, method := range []string{"PUT", "DELETE"} {
		if res := s.do(s.owner, method, path, `{}`); res.StatusCode != 409 || s.errorCode(res) != "ticket_void" {
			t.Errorf("%s on a cancelled ticket: %d %q, want 409 ticket_void", method, res.StatusCode, s.errorCode(res))
		}
	}
}
