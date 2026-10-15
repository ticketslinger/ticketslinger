package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func (s shop) orderPath(order string) string {
	return "/v1/organizers/acme/events/" + s.f["event"] + "/orders/" + order
}

func (s shop) refundsOf(order string) int {
	s.t.Helper()
	return s.count(`select count(*) from refunds r join payments p on p.id = r.payment_id where p.order_id = $1`, order)
}

// errorField is the field a 4xx names, if any.
func errorField(body map[string]any) any {
	e, _ := body["error"].(map[string]any)
	return e["field"]
}

func (s shop) member(email, role string) string {
	s.t.Helper()
	var tok string
	if role == "admin-no-mfa" {
		tok, role = s.signupVerified(email), "admin"
	} else {
		tok = s.signupWithMFA(email)
	}
	if _, err := testdb.Owner(s.t, s.pool).Exec(context.Background(), `insert into memberships (organizer_id, user_id, role)
		select o.id, u.id, $2 from organizers o, users u where o.slug = 'acme' and u.email = $1`, email, role); err != nil {
		s.t.Fatal(err)
	}
	return tok
}

func TestOwnerRefundsOneTicket(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(2, "paid@example.com", "evt_refund_one")
	d := s.must(s.do(s.owner, "GET", s.orderPath(order), ""), 200)
	tickets := d["tickets"].([]any)
	if len(tickets) != 2 {
		t.Fatalf("tickets %v", tickets)
	}
	for _, tk := range tickets {
		if tk := tk.(map[string]any); tk["status"] != "valid" || tk["charge_cents"] != float64(2500) || tk["fee_cents"] != float64(100) || tk["refund_id"] != nil {
			t.Fatalf("ticket %v", tk)
		}
	}
	first := tickets[0].(map[string]any)["id"].(string)
	res := s.must(s.do(s.owner, "POST", s.orderPath(order)+"/refunds", `{"ticket_ids":["`+first+`"],"refund_fee":true}`), 201)
	refund, _ := res["refund"].(map[string]any)
	if res["amount_cents"] != float64(2600) || res["fee_cents"] != float64(100) || res["order_status"] != "partially_refunded" ||
		refund == nil || refund["status"] != "requested" || refund["amount_cents"] != float64(2600) || refund["fee_back"] != true {
		t.Fatalf("refund %v", res)
	}
	for _, o := range s.jsonList(s.do(s.owner, "GET", "/v1/organizers/acme/events/"+s.f["event"]+"/orders", "")) {
		if o := o.(map[string]any); o["id"] == order && (o["ticket_count"] != float64(1) || o["refunded_cents"] != float64(2600) ||
			o["fee_refunded_cents"] != float64(100) || o["status"] != "partially_refunded") {
			t.Fatalf("listed %v", o)
		}
	}
	_, key := s.createKey(s.owner, "acme", "read")
	d = s.must(s.do(key, "GET", s.orderPath(order), ""), 200)
	tk := d["tickets"].([]any)[0].(map[string]any)
	refunds := d["refunds"].([]any)
	if tk["status"] != "void" || tk["refund_id"] != refund["id"] || len(refunds) != 1 || refunds[0].(map[string]any)["id"] != refund["id"] {
		t.Fatalf("detail after the refund %v", d)
	}
	if res := s.do(s.owner, "GET", s.orderPath("00000000-0000-0000-0000-000000000000"), ""); res.StatusCode != 404 {
		t.Fatalf("unknown order: %d", res.StatusCode)
	}
}

func TestRefundsArePeopleOnly(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "paid@example.com", "evt_refund_people")
	_, writeKey := s.createKey(s.owner, "acme", "write")
	for _, c := range []struct{ who, code string }{
		{writeKey, "session_required"},
		{s.member("door@example.com", "checkin"), "forbidden_role"},
		{s.member("admin@example.com", "admin-no-mfa"), "mfa_required"},
	} {
		for _, path := range []string{s.orderPath(order) + "/refunds", s.orderPath(s.f["order"]) + "/refunds/" + s.f["refund"] + "/retry"} {
			res := s.do(c.who, "POST", path, `{"refund_fee":true}`)
			if res.StatusCode != 403 || s.errorCode(res) != c.code {
				t.Errorf("%s: got %d %q, want 403 %s", path, res.StatusCode, s.errorCode(res), c.code)
			}
		}
	}
	if s.refundsOf(order) != 0 || s.count(`select count(*) from refunds where status = 'failed'`) != 1 {
		t.Fatal("a refused caller changed a refund")
	}
}

func TestRefundBodyRules(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "paid@example.com", "evt_refund_body")
	other := s.buyPaid(1, "other@example.com", "evt_refund_other")
	theirs := s.must(s.do(s.owner, "GET", s.orderPath(other), ""), 200)["tickets"].([]any)[0].(map[string]any)["id"].(string)
	many := `"` + strings.Repeat(`00000000-0000-0000-0000-000000000000","`, 100) + `00000000-0000-0000-0000-000000000001"`
	for _, c := range []struct{ name, body, code, field string }{
		{"no refund_fee", `{}`, "missing_field", "refund_fee"},
		{"unknown ticket", `{"ticket_ids":["00000000-0000-0000-0000-000000000000"],"refund_fee":false}`, "bad_tickets", "ticket_ids"},
		{"another order's ticket", `{"ticket_ids":["` + theirs + `"],"refund_fee":false}`, "bad_tickets", "ticket_ids"},
		{"an empty list", `{"ticket_ids":[],"refund_fee":false}`, "bad_tickets", "ticket_ids"},
		{"a null list", `{"ticket_ids":null,"refund_fee":false}`, "bad_tickets", "ticket_ids"},
		{"over 100 tickets", `{"ticket_ids":[` + many + `],"refund_fee":false}`, "too_many", "ticket_ids"},
	} {
		res := s.do(s.owner, "POST", s.orderPath(order)+"/refunds", c.body)
		body := s.json(res)
		if res.StatusCode != 422 || body["error"].(map[string]any)["code"] != c.code || errorField(body) != c.field {
			t.Errorf("%s: got %d %v, want 422 %s on %s", c.name, res.StatusCode, body, c.code, c.field)
		}
	}
	if s.refundsOf(order) != 0 {
		t.Fatal("a refused body wrote a refund")
	}
	s.must(s.do(s.owner, "POST", s.orderPath(order)+"/refunds", `{"refund_fee":false}`), 201)
	if res := s.do(s.owner, "POST", s.orderPath(order)+"/refunds", `{"refund_fee":false}`); res.StatusCode != 409 || s.errorCode(res) != "not_refundable" {
		t.Fatalf("refunding it again: %d %q, want 409 not_refundable", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(s.owner, "POST", s.orderPath(other)+"/refunds/"+s.f["refund"]+"/retry", ""); res.StatusCode != 404 {
		t.Fatalf("retrying another order's refund: %d, want 404", res.StatusCode)
	}
}

// "Try again" answers with the same refund, back to requested.
func TestRetryRefundOverHTTP(t *testing.T) {
	s := newShop(t, nil)
	path := s.orderPath(s.f["order"]) + "/refunds/" + s.f["refund"] + "/retry"
	r := s.must(s.do(s.owner, "POST", path, ""), 200)
	if r["id"] != s.f["refund"] || r["status"] != "requested" || r["failure"] != nil || r["amount_cents"] != float64(100) {
		t.Fatalf("retried %v", r)
	}
	if n := s.count(`select count(*) from refunds`); n != 1 {
		t.Fatalf("%d refund rows, want the one", n)
	}
	if res := s.do(s.owner, "POST", path, ""); res.StatusCode != 409 || s.errorCode(res) != "not_failed" {
		t.Fatalf("again: %d %q, want 409 not_failed", res.StatusCode, s.errorCode(res))
	}
}

func TestCancelEventOverHTTP(t *testing.T) {
	s := newShop(t, nil)
	s.buyPaid(1, "one@example.com", "evt_cancel_one")
	s.buyPaid(2, "two@example.com", "evt_cancel_two")
	open := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)["token"].(string)
	path := "/v1/organizers/acme/events/" + s.f["event"] + "/cancel"
	_, writeKey := s.createKey(s.owner, "acme", "write")
	if res := s.do(writeKey, "POST", path, `{"confirm_name":"Gala"}`); res.StatusCode != 403 || s.errorCode(res) != "session_required" {
		t.Fatalf("write key: %d %q, want 403 session_required", res.StatusCode, s.errorCode(res))
	}
	for _, body := range []string{`{"confirm_name":"gala"}`, `{"confirm_name":"` + strings.Repeat("G", 201) + `"}`} {
		res := s.do(s.owner, "POST", path, body)
		if b := s.json(res); res.StatusCode != 422 || (b["error"].(map[string]any)["code"] != "confirm_mismatch" || errorField(b) != "confirm_name") {
			t.Fatalf("%.40s: %d %v, want 422 confirm_mismatch", body, res.StatusCode, b)
		}
	}
	if n := s.count(`select count(*) from refunds where reason = 'event_cancelled'`); n != 0 {
		t.Fatal("a refused cancel refunded")
	}
	res := s.must(s.do(s.owner, "POST", path, `{"confirm_name":" Gala "}`), 200)
	ev, _ := res["event"].(map[string]any)
	if res["orders"] != float64(2) || res["amount_cents"] != float64(3*2600) || res["fee_cents"] != float64(300) || ev["status"] != "cancelled" {
		t.Fatalf("cancelled %v", res)
	}
	if r := s.send("", "", "GET", "/v1/storefront/acme/gala", ""); r.StatusCode != 404 {
		t.Fatalf("storefront after cancelling: %d, want 404", r.StatusCode)
	}
	// A buyer still at the checkout is told why, not shown a missing page.
	for _, r := range []*http.Response{s.send("", "", "GET", "/v1/checkouts/"+open, ""), s.pay(open)} {
		if r.StatusCode != 409 || s.errorCode(r) != "event_cancelled" {
			t.Fatalf("%s %s after cancelling: %d %q, want 409 event_cancelled", r.Request.Method, r.Request.URL.Path, r.StatusCode, s.errorCode(r))
		}
	}
	if r := s.do(s.owner, "POST", path, `{"confirm_name":"Gala"}`); r.StatusCode != 409 || s.errorCode(r) != "event_not_live" {
		t.Fatalf("again: %d %q, want 409 event_not_live", r.StatusCode, s.errorCode(r))
	}
	if r := s.do(s.owner, "PATCH", "/v1/organizers/acme/events/"+s.f["event"], `{"status":"live"}`); r.StatusCode != 422 || s.errorCode(r) != "status_not_allowed" {
		t.Fatalf("back to live: %d %q, want 422 status_not_allowed", r.StatusCode, s.errorCode(r))
	}
}
