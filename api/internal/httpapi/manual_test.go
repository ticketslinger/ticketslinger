package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// doKey is do with an Idempotency-Key.
func (h *harness) doKey(token, method, path, key, body string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (s shop) ordersPath() string { return "/v1/organizers/acme/events/" + s.f["event"] + "/orders" }

func (s shop) compBody(email string) string {
	return `{"kind":"comp","email":"` + email + `","lines":[{"ticket_type_id":"` + s.f["ticketType"] + `","quantity":1}]}`
}

func (s shop) addOrder(token, body string) *http.Response {
	s.t.Helper()
	return s.doKey(token, "POST", s.ordersPath(), uuid.NewString(), body)
}

func TestCreateManualOrderNeedsAKey(t *testing.T) {
	s := newShop(t, nil)
	if res := s.do(s.owner, "POST", s.ordersPath(), s.compBody("nokey@example.com")); res.StatusCode != 400 || s.errorCode(res) != "idempotency_key_required" {
		t.Fatalf("no key: %d %q, want 400 idempotency_key_required", res.StatusCode, s.errorCode(res))
	}
	if n := s.count(`select count(*) from orders where email = 'nokey@example.com'`); n != 0 {
		t.Fatalf("no key made %d orders", n)
	}
	a := s.doKey(s.owner, "POST", s.ordersPath(), "manual-1", s.compBody("once@example.com"))
	b := s.doKey(s.owner, "POST", s.ordersPath(), "manual-1", s.compBody("once@example.com"))
	ab, _ := io.ReadAll(a.Body)
	bb, _ := io.ReadAll(b.Body)
	if a.StatusCode != 201 || b.StatusCode != 201 || string(ab) != string(bb) {
		t.Fatalf("first %d %s; again %d %s", a.StatusCode, ab, b.StatusCode, bb)
	}
	if n := s.count(`select count(*) from orders where email = 'once@example.com'`); n != 1 {
		t.Fatalf("one key made %d orders", n)
	}
	if res := s.doKey(s.owner, "POST", s.ordersPath(), "manual-1", s.compBody("other@example.com")); res.StatusCode != 422 || s.errorCode(res) != "idempotency_key_reused" {
		t.Fatalf("same key, other body: %d %q, want 422 idempotency_key_reused", res.StatusCode, s.errorCode(res))
	}
}

// Staff mode and suspension are refused for every write by StaffModeWalk and
// SuspensionWalk, which pick createManualOrder up from the spec.
func TestCreateManualOrderRoles(t *testing.T) {
	s := newShop(t, nil)
	_, writeKey := s.createKey(s.owner, "acme", "write")
	for _, c := range []struct {
		who, code string
	}{
		{writeKey, "session_required"},
		{s.member("door@example.com", "checkin"), "forbidden_role"},
		{s.member("nomfa@example.com", "admin-no-mfa"), "mfa_required"},
	} {
		if res := s.addOrder(c.who, s.compBody("refused@example.com")); res.StatusCode != 403 || s.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q", c.code, res.StatusCode, s.errorCode(res))
		}
	}
	if n := s.count(`select count(*) from orders where email = 'refused@example.com'`); n != 0 {
		t.Fatalf("refused callers made %d orders", n)
	}
	for _, who := range []string{s.owner, s.member("admin@example.com", "admin")} {
		b := s.must(s.addOrder(who, s.compBody("given@example.com")), 201)
		if code, _ := b["code"].(string); !strings.HasPrefix(code, "TS-") || b["id"] == nil {
			t.Fatalf("created %v", b)
		}
	}
}

func TestCreateManualOrderRateLimit(t *testing.T) {
	s := newShop(t, nil)
	for range 20 {
		s.must(s.addOrder(s.owner, s.compBody("busy@example.com")), 201)
	}
	if res := s.addOrder(s.owner, s.compBody("busy@example.com")); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("21st: %d %q, want 429 rate_limited", res.StatusCode, s.errorCode(res))
	}
	s.must(s.addOrder(s.member("admin@example.com", "admin"), s.compBody("busy@example.com")), 201)
}

func TestManualWithoutAmount(t *testing.T) {
	s := newShop(t, nil)
	res := s.addOrder(s.owner, `{"kind":"manual","method":"cash","email":"cash@example.com","lines":[{"ticket_type_id":"`+s.f["ticketType"]+`","quantity":1}]}`)
	if b := s.json(res); res.StatusCode != 422 || errorField(b) != "amount_cents" || b["error"].(map[string]any)["code"] != "missing_field" {
		t.Fatalf("manual with no amount: %d %v, want 422 missing_field amount_cents", res.StatusCode, b)
	}
}

func TestOrdersCarrySource(t *testing.T) {
	s := newShop(t, nil)
	online := s.buyPaid(1, "paid@example.com", "evt_source_online")
	manual := s.must(s.addOrder(s.owner, `{"kind":"manual","method":"cheque","amount_cents":2000,"email":"cheque@example.com","buyer_name":" Dana Ruiz ",
		"send_tickets":false,"lines":[{"ticket_type_id":"`+s.f["ticketType"]+`","quantity":1}]}`), 201)["id"].(string)
	byID := map[string]map[string]any{}
	for _, o := range s.jsonList(s.do(s.owner, "GET", s.ordersPath(), "")) {
		byID[o.(map[string]any)["id"].(string)] = o.(map[string]any)
	}
	for _, o := range []map[string]any{byID[online], s.must(s.do(s.owner, "GET", s.orderPath(online), ""), 200)} {
		if o["source"] != "online" || o["manual_method"] != nil || o["buyer_name"] != "" {
			t.Fatalf("online order %v", o)
		}
	}
	for _, o := range []map[string]any{byID[manual], s.must(s.do(s.owner, "GET", s.orderPath(manual), ""), 200)} {
		if o["source"] != "manual" || o["manual_method"] != "cheque" || o["buyer_name"] != "Dana Ruiz" || o["total_cents"] != float64(2000) ||
			o["fee_cents"] != float64(0) || o["ticket_count"] != float64(1) {
			t.Fatalf("manual order %v", o)
		}
	}
}
