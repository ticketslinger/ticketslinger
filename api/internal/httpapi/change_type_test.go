package httpapi_test

import "testing"

// Staff mode and suspension are refused for every write by StaffModeWalk and
// SuspensionWalk, which pick changeTicketType up from the spec.
func TestChangeTicketTypeOverHTTP(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	base := "/v1/organizers/acme/events/" + s.f["event"]
	q := s.must(s.do(s.owner, "POST", base+"/quotas", `{"name":"VIP","size":5,"ticket_type_ids":[]}`), 201)["id"].(string)
	vip := s.must(s.do(s.owner, "POST", base+"/ticket-types", `{"name":"VIP","price_cents":2500,"quota_ids":["`+q+`"]}`), 201)["id"].(string)
	order, code, token := s.buyLinked(1, "retype@example.com", "evt_retype")
	_, tickets := s.ticketsOf(code, token)
	path := s.orderPath(order) + "/tickets/" + tickets[0]["id"].(string) + "/type"
	toVIP, toGeneral := `{"ticket_type_id":"`+vip+`"}`, `{"ticket_type_id":"`+s.f["ticketType"]+`"}`

	for _, c := range []struct{ who, code string }{
		{writeKey, "session_required"},
		{s.member("door@example.com", "checkin"), "forbidden_role"},
		{s.member("nomfa@example.com", "admin-no-mfa"), "mfa_required"},
	} {
		if res := s.do(c.who, "PUT", path, toVIP); res.StatusCode != 403 || s.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q, want 403", c.code, res.StatusCode, s.errorCode(res))
		}
	}
	if res := s.do(s.owner, "PUT", path, toGeneral); res.StatusCode != 422 || s.errorCode(res) != "same_type" {
		t.Fatalf("same type: %d %q, want 422 same_type", res.StatusCode, s.errorCode(res))
	}
	if got := s.must(s.do(s.owner, "PUT", path, toVIP), 200); got["email"] != "on_its_way" {
		t.Fatalf("change: %v", got)
	}
	if _, tickets = s.ticketsOf(code, token); tickets[0]["ticket_type_name"] != "VIP" {
		t.Fatalf("buyer's page after the change: %v", tickets[0])
	}
	s.must(s.do(admin, "PUT", path, toGeneral), 200)
}
