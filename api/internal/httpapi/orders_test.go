package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListOrders(t *testing.T) {
	s := newShop(t, nil)
	path := "/v1/organizers/acme/events/" + s.f["event"] + "/orders"
	buy := func(qty int, email, evt string, settle bool) {
		t.Helper()
		token := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], qty, "")), 201)["token"].(string)
		pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"`+email+`"}`), 200)
		if !settle {
			return
		}
		pi, _, _ := strings.Cut(pay["client_secret"].(string), "_secret_")
		s.must(s.webhook(event(evt, "payment_intent.succeeded", map[string]any{"id": pi, "object": "payment_intent",
			"amount": pay["amount_cents"], "currency": "usd", "status": "succeeded"}), webhookSecret, time.Now()), 200)
	}
	if res := s.do(s.owner, "GET", path, ""); res.StatusCode != 200 || len(s.jsonList(res)) != 0 {
		t.Fatalf("before any sale: %d", res.StatusCode)
	}
	buy(1, "pending@example.com", "", false)
	buy(2, "paid@example.com", "evt_orders_paid", true)

	_, key := s.createKey(s.owner, "acme", "read")
	for _, who := range []string{s.owner, key} {
		list := s.jsonList(s.do(who, "GET", path, ""))
		if len(list) != 1 {
			t.Fatalf("listed %d orders, want only the paid one: %v", len(list), list)
		}
		o := list[0].(map[string]any)
		if o["email"] != "paid@example.com" || o["status"] != "paid" || o["ticket_count"] != float64(2) || o["total_cents"] != float64(5200) ||
			o["fee_mode"] != "pass" || o["paid_at"] == nil {
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
