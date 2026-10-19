package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// buy checks out qty tickets for email and pays; with evt it also sends
// Stripe's payment_intent.succeeded as event evt.
func (s shop) buy(qty int, email, evt string) {
	s.t.Helper()
	token := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], qty, "")), 201)["token"].(string)
	pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"`+email+`"}`), 200)
	if evt == "" {
		return
	}
	pi, _, _ := strings.Cut(pay["client_secret"].(string), "_secret_")
	s.must(s.webhook(event(evt, "payment_intent.succeeded", map[string]any{"id": pi, "object": "payment_intent",
		"amount": pay["amount_cents"], "currency": "usd", "status": "succeeded"}), webhookSecret, time.Now()), 200)
}

// buyPaid is a paid order of qty tickets for email, settled by Stripe event evt.
func (s shop) buyPaid(qty int, email, evt string) (orderID string) {
	s.t.Helper()
	s.buy(qty, email, evt)
	if err := testdb.Owner(s.t, s.pool).QueryRow(context.Background(), `select id::text from orders where email = $1`, email).Scan(&orderID); err != nil {
		s.t.Fatal(err)
	}
	return orderID
}

func TestListOrders(t *testing.T) {
	s := newShop(t, nil)
	path := "/v1/organizers/acme/events/" + s.f["event"] + "/orders"
	// The fixture's own order (seed@example.com) is listed throughout.
	bought := func(who string) (out []map[string]any) {
		t.Helper()
		for _, o := range s.jsonList(s.do(who, "GET", path, "")) {
			if o := o.(map[string]any); o["email"] != "seed@example.com" {
				out = append(out, o)
			}
		}
		return out
	}
	if list := bought(s.owner); len(list) != 0 {
		t.Fatalf("before any sale: %v", list)
	}
	s.buy(1, "pending@example.com", "")
	s.buyPaid(2, "paid@example.com", "evt_orders_paid")

	_, key := s.createKey(s.owner, "acme", "read")
	for _, who := range []string{s.owner, key} {
		list := bought(who)
		if len(list) != 1 {
			t.Fatalf("listed %d orders, want only the paid one: %v", len(list), list)
		}
		o := list[0]
		if o["email"] != "paid@example.com" || o["status"] != "paid" || o["ticket_count"] != float64(2) || o["total_cents"] != float64(5200) ||
			o["fee_mode"] != "pass" || o["paid_at"] == nil || o["refunded_cents"] != float64(0) || o["fee_refunded_cents"] != float64(0) {
			t.Fatalf("order = %v", o)
		}
	}
	if res := s.do(s.owner, "GET", "/v1/organizers/acme/events/00000000-0000-0000-0000-000000000000/orders", ""); res.StatusCode != 404 {
		t.Fatalf("unknown event: %d", res.StatusCode)
	}
}

// jsonList reads a JSON array body.
func (h *harness) jsonList(res *http.Response) []any {
	h.t.Helper()
	b, _ := io.ReadAll(res.Body)
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("%d %s", res.StatusCode, b)
	}
	return out
}

func TestResendTicketEmailAPI(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "buyer@example.com", "evt_resend_paid")
	path := s.orderPath(order) + "/email"
	root := testdb.Owner(t, s.pool)
	sentAgo := func(minutes int) {
		t.Helper()
		if _, err := root.Exec(context.Background(), `update orders set mail_status = 'sent',
			mail_requested_at = now() - make_interval(mins => $2) where id = $1`, order, minutes); err != nil {
			t.Fatal(err)
		}
	}
	conflict := func(who, code string) {
		t.Helper()
		if res := s.do(who, "POST", path, ""); res.StatusCode != 409 || s.errorCode(res) != code {
			t.Fatalf("%d %q, want 409 %s", res.StatusCode, s.errorCode(res), code)
		}
	}
	conflict(s.owner, "email_on_its_way") // the first one is still queued

	_, writeKey := s.createKey(s.owner, "acme", "write")
	for _, who := range []string{s.owner, writeKey} {
		sentAgo(11)
		r := s.must(s.do(who, "POST", path, ""), 202)
		if r["status"] != "queued" || r["problem"] != nil || r["attempts"] != float64(0) || r["updated_at"] == nil {
			t.Fatalf("resent: %v", r)
		}
		conflict(who, "email_on_its_way")
	}
	sentAgo(5)
	conflict(s.owner, "too_soon")

	_, readKey := s.createKey(s.owner, "acme", "read")
	sentAgo(11)
	for _, who := range []string{readKey, s.member("door@example.com", "checkin")} {
		if res := s.do(who, "POST", path, ""); res.StatusCode != 403 {
			t.Fatalf("%d, want 403", res.StatusCode)
		}
	}
	if _, err := root.Exec(context.Background(), `update tickets set status = 'void' where order_id = $1`, order); err != nil {
		t.Fatal(err)
	}
	conflict(s.owner, "no_tickets")

	d := s.must(s.do(s.owner, "GET", s.orderPath(order), ""), 200)
	if e, _ := d["ticket_email"].(map[string]any); e["status"] != "sent" {
		t.Fatalf("order detail ticket_email %v, want sent", d["ticket_email"])
	}
}
