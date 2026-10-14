package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

const webhookSecret = "whsec_test"

// event is a Stripe event body from acme's account (acct_acme).
func event(id, typ string, object map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "api_version": stripe.APIVersion,
		"account": "acct_acme", "created": time.Now().Unix(), "data": map[string]any{"object": object}})
	return b
}

// webhook posts body to the Stripe webhook as Stripe would, signed with
// secret at at. The route is outside the spec (it needs the raw body), so
// this goes around the validating client.
func (s shop) webhook(body []byte, secret string, at time.Time) *http.Response {
	s.t.Helper()
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret, Timestamp: at})
	req, _ := http.NewRequest("POST", s.srv.URL+"/v1/webhooks/stripe", bytes.NewReader(signed.Payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	res, err := s.srv.Client().Do(req) // not in the spec: Stripe's own format
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestStripeWebhookChecksTheSignature(t *testing.T) {
	s := newShop(t, nil)
	owner := testdb.Owner(t, s.pool)
	ctx := context.Background()
	if _, err := owner.Exec(ctx, `update organizers set stripe_charges_enabled = false where slug = 'acme'`); err != nil {
		t.Fatal(err)
	}
	updated := func(id string) []byte {
		return event(id, "account.updated", map[string]any{"id": "acct_acme", "object": "account", "charges_enabled": true, "details_submitted": true})
	}
	old := event("evt_old", "account.updated", nil)
	old = bytes.Replace(old, []byte(stripe.APIVersion), []byte("2020-08-27"), 1)
	for name, res := range map[string]*http.Response{
		"wrong secret":          s.webhook(updated("evt_1"), "whsec_other", time.Now()),
		"signed 10 minutes ago": s.webhook(updated("evt_1"), webhookSecret, time.Now().Add(-10*time.Minute)),
		"older API version":     s.webhook(old, webhookSecret, time.Now()),
	} {
		if res.StatusCode != 400 || s.errorCode(res) != "bad_signature" {
			t.Errorf("%s: %d %q, want 400 bad_signature", name, res.StatusCode, s.errorCode(res))
		}
	}
	var enabled bool
	if err := owner.QueryRow(ctx, `select stripe_charges_enabled from organizers where slug = 'acme'`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("a refused event changed the organizer: %v %v", enabled, err)
	}
	if b := s.must(s.webhook(updated("evt_1"), webhookSecret, time.Now()), 200); b["received"] != true {
		t.Fatalf("good signature: %v", b)
	}
	var details bool
	if err := owner.QueryRow(ctx, `select stripe_charges_enabled, stripe_details_submitted from organizers where slug = 'acme'`).Scan(&enabled, &details); err != nil || !enabled || !details {
		t.Fatalf("account.updated: charges %v details %v %v, want both on", enabled, details, err)
	}
	// Anything else is recorded and ignored.
	s.must(s.webhook(event("evt_2", "customer.created", map[string]any{"id": "cus_1"}), webhookSecret, time.Now()), 200)
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from stripe_events`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d events recorded, want 2 (%v)", n, err)
	}
}

func TestStripeWebhookIsOffWithoutASecret(t *testing.T) {
	h := httpapi.New(httpapi.Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/webhooks/stripe", strings.NewReader(`{}`)))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("got %d %s, want the API's own 404", rec.Code, rec.Body)
	}
}

// From the buyer's side: pay, Stripe says it went through, and the link in
// the order opens the order page, privately.
func TestPaidOrderOverHTTP(t *testing.T) {
	s := newShop(t, nil)
	token := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 2, "")), 201)["token"].(string)
	pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com","attendees":["Ada"]}`), 200)
	pi, _, _ := strings.Cut(pay["client_secret"].(string), "_secret_")
	s.must(s.webhook(event("evt_paid", "payment_intent.succeeded", map[string]any{"id": pi, "object": "payment_intent",
		"amount": pay["amount_cents"], "currency": "usd", "status": "succeeded"}), webhookSecret, time.Now()), 200)
	c := s.must(s.send("", "", "GET", "/v1/checkouts/"+token, ""), 200)
	link, err := url.Parse(c["order_url"].(string))
	if err != nil || c["status"] != "completed" {
		t.Fatalf("checkout after payment: %v", c)
	}
	code, secret := c["order_code"].(string), link.Query().Get("t")

	read := func(ip, code, secret string) *http.Response {
		return s.send(ip, "", "GET", "/v1/orders/"+code+"?token="+url.QueryEscape(secret), "")
	}
	res := read("", code, secret)
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers %v, want no-store and no-referrer", res.Header)
	}
	o := s.must(res, 200)
	tickets, _ := o["tickets"].([]any)
	if o["code"] != code || o["status"] != "paid" || len(tickets) != 2 || o["event"].(map[string]any)["organizer_name"] != "acme" {
		t.Fatalf("order = %v", o)
	}
	first := tickets[0].(map[string]any)
	if qr, _ := first["qr"].(string); len(qr) != 22 || first["attendee_name"] != "Ada" || first["ticket_type_name"] != "General" {
		t.Fatalf("ticket = %v", first)
	}
	body := func(res *http.Response) string {
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	wrong, unknown := read("", code, "wrong"), read("", "TS-00000", secret)
	if wb, ub := body(wrong), body(unknown); wrong.StatusCode != 404 || unknown.StatusCode != 404 || wb != ub {
		t.Fatalf("wrong token %d %s; unknown code %d %s; want the same 404", wrong.StatusCode, wb, unknown.StatusCode, ub)
	}
	// 60 reads a minute from one address.
	for range 60 {
		s.must(read("203.0.113.7", code, "wrong"), 404)
	}
	if res := read("203.0.113.7", code, secret); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("61st read: %d %q, want 429", res.StatusCode, s.errorCode(res))
	}
	s.must(read("198.51.100.4", code, secret), 200)
}
