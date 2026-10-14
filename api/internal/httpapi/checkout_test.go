package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
)

// shop is acme's live event gala, selling General ($25) from quota Hall
// (100), behind a trusted local proxy, with the fake payment provider.
type shop struct {
	*harness
	owner string
	f     map[string]string
}

func newShop(t *testing.T, v *turnstile.Verifier) shop {
	pool := testdb.New(t)
	m := &mailer.Memory{}
	svc := auth.NewService(pool, m, auth.Options{RPID: "localhost", Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	cfg := config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")},
		StorefrontURL: "http://localhost:3003", SecretKey: []byte("0123456789abcdef0123456789abcdef"), PlatformFeeBP: 200, PlatformFeeCents: 50,
		StripeWebhookSecret: webhookSecret}
	srv := startServer(t, httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Payments: payments.NewFake(), Config: cfg, Turnstile: v}))
	h := &harness{t, srv, m, pool, clientFor(t, srv.URL)}
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.chargesEnabled("acme")
	f := seedCatalogue(h, owner, "acme", "")
	h.must(h.do(owner, "PATCH", "/v1/organizers/acme/events/"+f["event"], `{"status":"live"}`), 200)
	return shop{h, owner, f}
}

// send is do without a session, from ip (via the trusted proxy), with an
// optional Idempotency-Key.
func (s shop) send(ip, key, method, path, body string) *http.Response {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (s shop) cart(tt string, n int, extra string) string {
	b, _ := json.Marshal(map[string]any{"ticket_type_id": tt, "quantity": n})
	return `{"organizer":"acme","event":"gala","items":[` + string(b) + `]` + extra + `}`
}

func TestCheckoutOverHTTP(t *testing.T) {
	s := newShop(t, nil)
	ev := s.must(s.send("", "", "GET", "/v1/storefront/acme/gala", ""), 200)
	types := ev["ticket_types"].([]any)
	if len(types) != 1 || types[0].(map[string]any)["remaining"] != float64(100) || ev["promo"] != nil {
		t.Fatalf("storefront = %v", ev)
	}
	if res := s.send("", "", "GET", "/v1/storefront/acme/nope", ""); res.StatusCode != 404 {
		t.Fatalf("unknown event: %d", res.StatusCode)
	}

	c := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 2, "")), 201)
	token, _ := c["token"].(string)
	if len(token) != 43 || c["status"] != "open" || c["quote"].(map[string]any)["total_cents"] != float64(5200) {
		t.Fatalf("checkout = %v", c)
	}
	got := s.must(s.send("", "", "GET", "/v1/checkouts/"+token, ""), 200)
	if _, ok := got["token"]; ok || got["id"] != c["id"] {
		t.Fatalf("get = %v, want the checkout without its token", got)
	}
	if res := s.send("", "", "GET", "/v1/checkouts/nope", ""); res.StatusCode != 404 {
		t.Fatalf("unknown token: %d", res.StatusCode)
	}
	if res := s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"not an email"}`); res.StatusCode != 422 || errField(s.json(res)) != "email" {
		t.Fatalf("bad email: %d", res.StatusCode)
	}
	pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com","attendees":["Ada","Bob"]}`), 200)
	if pay["kind"] != "test" || pay["client_secret"] == "" || pay["publishable_key"] != "pk_test_fake" || pay["account"] != "acct_acme" || pay["amount_cents"] != float64(5200) {
		t.Fatalf("pay = %v", pay)
	}
	again := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`), 200)
	if again["client_secret"] != pay["client_secret"] {
		t.Fatalf("second pay got another payment: %v", again)
	}
	if got := s.must(s.send("", "", "GET", "/v1/checkouts/"+token, ""), 200); got["status"] != "paying" {
		t.Fatalf("after pay: %v", got)
	}

	// A free ticket is paid at once, and the checkout then points at the order.
	free := s.must(s.do(s.owner, "POST", "/v1/organizers/acme/events/"+s.f["event"]+"/ticket-types",
		`{"name":"Free","price_cents":0,"quota_ids":["`+s.f["quota"]+`"]}`), 201)["id"].(string)
	token = s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(free, 1, "")), 201)["token"].(string)
	pay = s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`), 200)
	url, _ := pay["order_url"].(string)
	if pay["kind"] != "free" || !strings.HasPrefix(url, "http://localhost:3003/orders/"+pay["order_code"].(string)+"?t=") {
		t.Fatalf("free pay = %v", pay)
	}
	if got := s.must(s.send("", "", "GET", "/v1/checkouts/"+token, ""), 200); got["status"] != "completed" || got["order_url"] != url {
		t.Fatalf("completed checkout = %v", got)
	}
	res := s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`)
	if e, _ := s.json(res)["error"].(map[string]any); res.StatusCode != 409 || e["code"] != "checkout_completed" || e["order_code"] != pay["order_code"] {
		t.Fatalf("paying twice: %d %v", res.StatusCode, e)
	}
}

func TestSoldOutSaysWhatsLeft(t *testing.T) {
	s := newShop(t, nil)
	s.must(s.do(s.owner, "PATCH", "/v1/organizers/acme/events/"+s.f["event"]+"/quotas/"+s.f["quota"], `{"size":1}`), 200)
	res := s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 2, ""))
	e, _ := s.json(res)["error"].(map[string]any)
	avail, _ := e["available"].([]any)
	if res.StatusCode != 409 || e["code"] != "sold_out" || len(avail) != 1 ||
		avail[0].(map[string]any)["ticket_type_id"] != s.f["ticketType"] || avail[0].(map[string]any)["remaining"] != float64(1) {
		t.Fatalf("sold out: %d %v", res.StatusCode, e)
	}
}

func TestCheckoutsAreRateLimitedPerAddress(t *testing.T) {
	s := newShop(t, nil)
	for i := 0; i < 10; i++ {
		s.must(s.send("203.0.113.9", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)
	}
	if res := s.send("203.0.113.9", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("11th checkout: %d %q, want 429", res.StatusCode, s.errorCode(res))
	}
	// One IPv6 /64 is one address.
	for i := 0; i < 10; i++ {
		s.must(s.send(fmt.Sprintf("2001:db8:7:7::%x", i+1), "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)
	}
	if res := s.send("2001:db8:7:7::ff", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")); res.StatusCode != 429 {
		t.Fatalf("11th from the same /64: %d, want 429", res.StatusCode)
	}
	s.must(s.send("198.51.100.4", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)
}

func TestCheckoutNeedsTurnstileWhenConfigured(t *testing.T) {
	cf := httptest.NewServer(http.HandlerFunc(acceptOK)) // raw: stands in for Cloudflare, not the API
	t.Cleanup(cf.Close)
	s := newShop(t, &turnstile.Verifier{Secret: turnstileSecret, URL: cf.URL, Client: cf.Client()})
	for _, extra := range []string{"", `,"turnstile_token":"forged"`} {
		res := s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, extra))
		if b := s.json(res); res.StatusCode != 422 || errCode(b) != "turnstile_failed" || errField(b) != "turnstile_token" {
			t.Fatalf("token %q: %d %v", extra, res.StatusCode, b)
		}
	}
	s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, `,"turnstile_token":"ok"`)), 201)
	cf.Close() // Cloudflare unreachable: refuse rather than let it through
	if res := s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, `,"turnstile_token":"ok"`)); res.StatusCode != 503 || s.errorCode(res) != "turnstile_unavailable" {
		t.Fatalf("siteverify down: %d %q", res.StatusCode, s.errorCode(res))
	}
}

// createCheckout keeps no response: the body holds the plaintext token. A
// duplicate in flight is still refused, and a retry later runs again.
func TestCheckoutTokensAreNeverKept(t *testing.T) {
	s := newShop(t, nil)
	ctx := context.Background()
	owner := testdb.Owner(t, s.pool)
	// Hold Hall's lock so the first checkout waits inside the handler.
	lock, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `select 1 from quotas for update`); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	type answer struct {
		status int
		body   map[string]any
	}
	done := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest("POST", s.srv.URL+"/v1/checkouts", strings.NewReader(s.cart(s.f["ticketType"], 1, "")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		res, err := s.client.Do(req)
		if err != nil {
			done <- answer{}
			return
		}
		defer func() { _ = res.Body.Close() }()
		var b map[string]any
		_ = json.NewDecoder(res.Body).Decode(&b)
		done <- answer{res.StatusCode, b}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := owner.QueryRow(ctx, `select count(*) from pg_locks where not granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first checkout never waited on the quota lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res := s.send("", key, "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")); res.StatusCode != 409 || s.errorCode(res) != "idempotency_in_progress" {
		t.Fatalf("duplicate in flight: %d %q, want 409 idempotency_in_progress", res.StatusCode, s.errorCode(res))
	}
	_ = lock.Rollback(ctx)
	first := <-done
	token, _ := first.body["token"].(string)
	if first.status != 201 || token == "" {
		t.Fatalf("first checkout: %d %v", first.status, first.body)
	}
	again := s.must(s.send("", key, "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)
	if again["token"] == token {
		t.Fatal("a retry replayed the first checkout's token")
	}
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from idempotency_keys
		where convert_from(coalesce(body, ''), 'UTF8') like '%' || $1 || '%' or convert_from(coalesce(body, ''), 'UTF8') like '%' || $2 || '%'`,
		token, again["token"]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d kept responses hold a checkout token (%v), want none", n, err)
	}
	// Pay keeps nothing either: its answer carries the client secret.
	payKey := uuid.NewString()
	a := s.must(s.send("", payKey, "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`), 200)
	res := s.send("", payKey, "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`)
	if b := s.must(res, 200); res.Header.Get("Idempotent-Replayed") != "" || b["client_secret"] != a["client_secret"] {
		t.Fatalf("pay retried with its key: replayed %q, %v", res.Header.Get("Idempotent-Replayed"), b)
	}
	if err := owner.QueryRow(ctx, `select count(*) from idempotency_keys where convert_from(coalesce(body, ''), 'UTF8') like '%secret%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d kept responses carry a secret (%v), want none", n, err)
	}
}

func TestStorefrontCodeGuessesAreRateLimited(t *testing.T) {
	s := newShop(t, nil)
	for i := 0; i < 30; i++ {
		s.must(s.send("203.0.113.5", "", "GET", fmt.Sprintf("/v1/storefront/acme/gala?code=GUESS%d", i), ""), 200)
	}
	if res := s.send("203.0.113.5", "", "GET", "/v1/storefront/acme/gala?code=EARLY", ""); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("31st code from one address: %d %q, want 429", res.StatusCode, s.errorCode(res))
	}
	s.must(s.send("203.0.113.5", "", "GET", "/v1/storefront/acme/gala", ""), 200) // no code, no limit
	if b := s.must(s.send("198.51.100.4", "", "GET", "/v1/storefront/acme/gala?code=early", ""), 200); b["promo"] == nil {
		t.Fatalf("another address with a good code: %v", b["promo"])
	}
}

func TestPaysAreRateLimitedPerAddress(t *testing.T) {
	s := newShop(t, nil)
	token := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)["token"].(string)
	for i := 0; i < 20; i++ {
		if res := s.send("203.0.113.6", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"nope"}`); res.StatusCode != 422 {
			t.Fatalf("pay %d: %d, want 422", i+1, res.StatusCode)
		}
	}
	if res := s.send("203.0.113.6", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("21st pay from one address: %d %q, want 429", res.StatusCode, s.errorCode(res))
	}
	s.must(s.send("198.51.100.4", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`), 200)
}
