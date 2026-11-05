package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// ticketsOf is the order page's tickets, read with the order's link.
func (s shop) ticketsOf(code, token string) (order map[string]any, tickets []map[string]any) {
	s.t.Helper()
	order = s.must(s.send("", "", "GET", "/v1/orders/"+code+"?token="+url.QueryEscape(token), ""), 200)
	for _, x := range order["tickets"].([]any) {
		tickets = append(tickets, x.(map[string]any))
	}
	return order, tickets
}

// renameByLink is the buyer's rename, from ip.
func (s shop) renameByLink(ip, code, ticket string, body map[string]any) *http.Response {
	s.t.Helper()
	b, _ := json.Marshal(body)
	return s.send(ip, "", "PATCH", "/v1/orders/"+code+"/tickets/"+ticket, string(b))
}

func readBody(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// An unknown code, a wrong token and a ticket of another order all answer
// what getOrder answers a wrong token, byte for byte.
func TestRenameByLinkIsNotFound(t *testing.T) {
	s := newShop(t, nil)
	_, code, token := s.buyLinked(1, "a@example.com", "evt_names_a")
	_, otherCode, otherToken := s.buyLinked(1, "b@example.com", "evt_names_b")
	_, mine := s.ticketsOf(code, token)
	_, theirs := s.ticketsOf(otherCode, otherToken)
	want := readBody(s.send("", "", "GET", "/v1/orders/"+code+"?token=wrong", ""))
	for label, try := range map[string]struct{ code, token, ticket string }{
		"wrong token":       {code, "wrong", mine[0]["id"].(string)},
		"unknown code":      {"TS-00000", token, mine[0]["id"].(string)},
		"other order's":     {code, token, theirs[0]["id"].(string)},
		"no such ticket":    {code, token, "0190a0a0-0000-7000-8000-000000000000"},
		"not even a code":   {"nope", token, mine[0]["id"].(string)},
		"other order's own": {otherCode, token, theirs[0]["id"].(string)},
	} {
		res := s.renameByLink("", try.code, try.ticket, map[string]any{"token": try.token, "attendee_name": "Ada"})
		if got := readBody(res); res.StatusCode != 404 || got != want {
			t.Errorf("%s: %d %s, want 404 %s", label, res.StatusCode, got, want)
		}
	}
	if _, tickets := s.ticketsOf(otherCode, otherToken); tickets[0]["attendee_name"] != "" {
		t.Fatalf("another order's ticket was renamed: %v", tickets[0])
	}
}

func TestRenameByLinkAnswers(t *testing.T) {
	s := newShop(t, nil)
	orderID, code, token := s.buyLinked(2, "a@example.com", "evt_names_c")
	order, tickets := s.ticketsOf(code, token)
	until, _ := time.Parse(time.RFC3339, fmt.Sprint(order["names_until"]))
	if !until.Equal(time.Date(2027, 3, 1, 19, 0, 0, 0, time.UTC)) || tickets[0]["name_changes_left"] != float64(5) {
		t.Fatalf("before: names_until %v, name_changes_left %v", order["names_until"], tickets[0]["name_changes_left"])
	}
	id := tickets[0]["id"].(string)
	if res := s.renameByLink("", code, id, map[string]any{"token": token, "attendee_name": " Ada Lovelace "}); res.StatusCode != 204 {
		t.Fatalf("rename: %d %s", res.StatusCode, readBody(res))
	}
	_, tickets = s.ticketsOf(code, token)
	if tickets[0]["attendee_name"] != "Ada Lovelace" || tickets[0]["name_changes_left"] != float64(4) || tickets[1]["name_changes_left"] != float64(5) {
		t.Fatalf("after: %v", tickets)
	}
	// The organizer's change after it, and the audit log telling them apart.
	s.must(s.do(s.owner, "PATCH", s.orderPath(orderID)+"/tickets/"+id, `{"attendee_name":"Ada King"}`), 204)
	var renames []map[string]any
	for _, x := range s.jsonList(s.do(s.owner, "GET", "/v1/organizers/acme/audit", "")) {
		if e := x.(map[string]any); e["action"] == "ticket.renamed" {
			renames = append(renames, e)
		} else if e["name_change"] != nil || e["by_buyer"] != nil {
			t.Errorf("%s carries a name change: %v", e["action"], e)
		}
	}
	if len(renames) != 2 || renames[0]["actor_type"] != "user" || renames[0]["by_buyer"] != false ||
		!jsonEqual(renames[0]["name_change"].(map[string]any), map[string]any{"from": "Ada Lovelace", "to": "Ada King"}) ||
		renames[1]["actor_type"] != "system" || renames[1]["by_buyer"] != true ||
		!jsonEqual(renames[1]["name_change"].(map[string]any), map[string]any{"from": "", "to": "Ada Lovelace"}) {
		t.Fatalf("audit renames: %v", renames)
	}

	invalid := func(body map[string]any, code_, field string) {
		t.Helper()
		res := s.renameByLink("", code, id, body)
		b := s.json(res)
		if res.StatusCode != 422 || errorField(b) != field || b["error"].(map[string]any)["code"] != code_ {
			t.Errorf("%v: %d %v, want 422 %s on %s", body, res.StatusCode, b, code_, field)
		}
	}
	invalid(map[string]any{"token": token, "attendee_name": "A\u202eda"}, "bad_attendee", "attendee_name")
	invalid(map[string]any{"token": token, "attendee_name": "A\u2028da"}, "bad_attendee", "attendee_name")
	invalid(map[string]any{"token": token, "attendee_name": strings.Repeat("a", 121)}, "bad_attendee", "attendee_name")
	invalid(map[string]any{"token": token}, "missing_field", "attendee_name")
	invalid(map[string]any{"token": strings.Repeat("a", 201), "attendee_name": "Ada"}, "bad_token", "token")
	if res := s.renameByLink("", code, id, map[string]any{"token": token, "attendee_name": "A\u202eda"}); s.json(res)["error"].(map[string]any)["message"] !=
		"Keep the name to 120 characters or fewer, using ordinary letters and punctuation." {
		t.Errorf("bad_attendee message: %v", res)
	}

	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update events set names_locked_at = now() - interval '1 minute' where id = $1`, s.f["event"]); err != nil {
		t.Fatal(err)
	}
	res := s.renameByLink("", code, id, map[string]any{"token": token, "attendee_name": "Bob"})
	if b := s.json(res); res.StatusCode != 409 || b["error"].(map[string]any)["code"] != "names_locked" {
		t.Fatalf("locked: %d %v, want 409 names_locked", res.StatusCode, b)
	}
	if order, _ := s.ticketsOf(code, token); order["names_until"] != nil {
		t.Fatalf("names_until %v once locked, want null", order["names_until"])
	}

	owner := testdb.Owner(t, s.pool)
	for _, q := range []string{`update events set names_locked_at = null where id = $1`, `update tickets set name_changes = 5 where event_id = $1`} {
		if _, err := owner.Exec(context.Background(), q, s.f["event"]); err != nil {
			t.Fatal(err)
		}
	}
	res = s.renameByLink("", code, id, map[string]any{"token": token, "attendee_name": "Bob"})
	if b := s.json(res); res.StatusCode != 409 || b["error"].(map[string]any)["code"] != "name_change_limit" {
		t.Fatalf("limit: %d %v, want 409 name_change_limit", res.StatusCode, b)
	}

	// Each try counts against the 60 order reads a minute.
	for range 60 {
		s.renameByLink("203.0.113.40", code, id, map[string]any{"token": "wrong", "attendee_name": "Ada"})
	}
	if res := s.renameByLink("203.0.113.40", code, id, map[string]any{"token": token, "attendee_name": "Ada"}); res.StatusCode != 429 {
		t.Fatalf("61st try: %d, want 429", res.StatusCode)
	}
}

// renamePath is the organizer's rename of ticket in the fixture's order.
func renamePath(f map[string]string, ticket string) string {
	return "/v1/organizers/acme/events/" + f["event"] + "/orders/" + f["order"] + "/tickets/" + ticket
}

func TestRenameTicketRoles(t *testing.T) {
	w := newStaffWorld(t)
	// Give the fixture's paid order a valid ticket.
	var ticket string
	if err := w.owner.QueryRow(context.Background(), `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents,
			tax_cents, fee_cents, charge_cents, secret)
		select organizer_id, event_id, id, $2, 2500, 0, 0, 100, 2500, decode(md5(random()::text), 'hex') from orders where id = $1
		returning id::text`, w.fixture["order"], w.fixture["ticketType"]).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	path := renamePath(w.fixture, ticket)
	admin := w.signupWithMFA("admin@example.com")
	w.member("admin@example.com", "acme", "admin")
	door := w.signupWithMFA("door@example.com")
	w.member("door@example.com", "acme", "checkin")
	_, readKey := w.createKey(w.ownerA, "acme", "read")
	_, writeKey := w.createKey(w.ownerA, "acme", "write")
	name := func() (n string) {
		t.Helper()
		if err := w.owner.QueryRow(context.Background(), `select attendee_name from tickets where id = $1`, ticket).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for who, tok := range map[string]string{"owner": w.ownerA, "admin": admin, "write key": writeKey} {
		if res := w.do(tok, "PATCH", path, `{"attendee_name":"`+who+`"}`); res.StatusCode != 204 || name() != who {
			t.Errorf("%s: %d %v, name %q", who, res.StatusCode, w.json(res), name())
		}
	}
	refused := func(who, tok, code string) {
		t.Helper()
		if res := w.do(tok, "PATCH", path, `{"attendee_name":"Mallory"}`); res.StatusCode != 403 || w.errorCode(res) != code {
			t.Errorf("%s: %d %q, want 403 %s", who, res.StatusCode, w.errorCode(res), code)
		}
	}
	refused("checkin", door, "forbidden_role")
	refused("read key", readKey, "scope_read_only")
	refused("staff mode", w.staffTok, "staff_read_only")
	if res := w.do(w.ownerA, "PATCH", path, `{}`); res.StatusCode != 422 || w.errorCode(res) != "missing_field" {
		t.Errorf("no name: %d %q, want 422 missing_field", res.StatusCode, w.errorCode(res))
	}
	if res := w.do(w.ownerA, "PATCH", path, `{"attendee_name":"A\u202eda"}`); res.StatusCode != 422 || w.errorCode(res) != "bad_attendee" {
		t.Errorf("bad name: %d %q, want 422 bad_attendee", res.StatusCode, w.errorCode(res))
	}
	if res := w.do(w.ownerA, "PATCH", renamePath(w.fixture, w.fixture["ticket"]), `{"attendee_name":"Ada"}`); res.StatusCode != 404 {
		t.Errorf("a ticket of another order: %d, want 404", res.StatusCode)
	}
	if _, err := w.owner.Exec(context.Background(), `update tickets set status = 'void' where id = $1`, ticket); err != nil {
		t.Fatal(err)
	}
	if res := w.do(w.ownerA, "PATCH", path, `{"attendee_name":"Ada"}`); res.StatusCode != 409 || w.errorCode(res) != "ticket_void" {
		t.Errorf("void: %d %q, want 409 ticket_void", res.StatusCode, w.errorCode(res))
	}
	if b := w.act(w.staffTok, "suspend"); b["status"] != 200 {
		t.Fatalf("suspend: %v", b)
	}
	refused("suspended owner", w.ownerA, "organizer_suspended")
	if name() != "write key" {
		t.Fatalf("a refused rename wrote %q", name())
	}
}
