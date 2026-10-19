package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/ticketslinger/ticketslinger/api/internal/apikeys"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func TestVersionIsSet(t *testing.T) {
	if version == "" {
		t.Fatal("version must default to a non-empty string")
	}
}

// local is a test-mode config that serve accepts.
func local(addr string) config.Config {
	return config.Config{HTTPAddr: addr, PublicURL: "http://127.0.0.1:3001", DashboardOrigin: "http://localhost:3002", Mail: "log"}
}

func get(t *testing.T, h http.Handler, path string, header ...string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestTestMailEndpointIsAbsentWithoutTestMode(t *testing.T) {
	h, _, err := newHandler(config.Config{HTTPAddr: "127.0.0.1:3000", TurnstileSecret: "s", Mail: "log"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, h, "/__test/last-email?to=a@example.com"); code != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("got %d %s, want the API's own 404", code, body)
	}
}

// Outside test mode, sign-in leans on Turnstile: no secret, no start,
// unless the operator says so out loud and the log says so too.
func TestServeNeedsTurnstileOutsideTestMode(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	bare := config.Config{HTTPAddr: "127.0.0.1:3000", Mail: "log"}
	t.Setenv("TS_ALLOW_NO_TURNSTILE", "")
	if _, _, err := newHandler(bare, nil, false); err == nil || !strings.Contains(err.Error(), "TS_TURNSTILE_SECRET") {
		t.Fatalf("no secret: %v, want a refusal naming TS_TURNSTILE_SECRET", err)
	}
	for _, v := range []string{"0", "true", "yes"} {
		t.Setenv("TS_ALLOW_NO_TURNSTILE", v)
		if _, _, err := newHandler(bare, nil, false); err == nil {
			t.Fatalf("TS_ALLOW_NO_TURNSTILE=%s started without Turnstile", v)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("refusals logged: %s", logs.String())
	}
	t.Setenv("TS_ALLOW_NO_TURNSTILE", "1")
	if _, _, err := newHandler(bare, nil, false); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "TS_ALLOW_NO_TURNSTILE") {
		t.Fatalf("allowed without a loud warning: %q", logs.String())
	}
	t.Setenv("TS_ALLOW_NO_TURNSTILE", "")
	if _, _, err := newHandler(local("127.0.0.1:3000"), nil, true); err != nil {
		t.Fatalf("test mode without a secret: %v", err)
	}
}

func TestTestModeRefusesNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3000", ":3000", "[::]:3000", "192.168.1.5:3000", "example.com:3000"} {
		if _, _, err := newHandler(local(addr), nil, true); err == nil {
			t.Errorf("%s: test mode started on a non-loopback address", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:3000", "[::1]:3000", "localhost:3000"} {
		if _, _, err := newHandler(local(addr), nil, true); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// Behind a same-host reverse proxy the API listens on loopback too, so the
// public URLs must be local as well before test mode will start.
func TestTestModeRefusesPublicURLs(t *testing.T) {
	for name, mod := range map[string]func(*config.Config){
		"https public URL":           func(c *config.Config) { c.PublicURL = "https://api.ticketslinger.com" },
		"https loopback public URL":  func(c *config.Config) { c.PublicURL = "https://127.0.0.1:3001" },
		"public dashboard origin":    func(c *config.Config) { c.DashboardOrigin = "http://app.ticketslinger.com" },
		"LAN dashboard origin":       func(c *config.Config) { c.DashboardOrigin = "http://192.168.1.5:3002" },
		"lookalike dashboard origin": func(c *config.Config) { c.DashboardOrigin = "http://localhost.example.com" },
	} {
		c := local("127.0.0.1:3001")
		mod(&c)
		if _, _, err := newHandler(c, nil, true); err == nil {
			t.Errorf("%s: test mode started", name)
		}
	}
}

func TestTestMailRefusesProxiedRequests(t *testing.T) {
	m := &mailer.Memory{}
	_ = m.Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Confirm", Text: "Open http://localhost:3002/verify-email?token=t"})
	h := testRoutes(http.NotFoundHandler(), m, nil, "")
	for _, hdr := range []string{"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if code, body := get(t, h, "/__test/last-email?to=a@example.com", hdr, "203.0.113.9"); code != 404 {
			t.Errorf("%s: got %d %q, want 404", hdr, code, body)
		}
	}
}

func TestTestMailReturnsTheNewestLinkForThatAddress(t *testing.T) {
	m := &mailer.Memory{}
	ctx := context.Background()
	_ = m.Send(ctx, mailer.Message{To: "a@example.com", Subject: "Confirm", Text: "Open http://localhost:3002/verify-email?token=old to confirm."})
	_ = m.Send(ctx, mailer.Message{To: "a@example.com", Subject: "Confirm", Text: "Open http://localhost:3002/verify-email?token=new to confirm."})
	_ = m.Send(ctx, mailer.Message{To: "b@example.com", Subject: "Confirm", Text: "Open http://localhost:3002/verify-email?token=bee to confirm."})
	h := testRoutes(http.NotFoundHandler(), m, nil, "")

	if code, body := get(t, h, "/__test/last-email?to=a@example.com"); code != 200 || body != "http://localhost:3002/verify-email?token=new" {
		t.Fatalf("got %d %q", code, body)
	}
	if code, _ := get(t, h, "/__test/last-email?to=nobody@example.com"); code != 404 {
		t.Fatalf("no mail: got %d, want 404", code)
	}
}

func TestTestMailSummary(t *testing.T) {
	m := &mailer.Memory{}
	_ = m.Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Your tickets", Parts: []mailer.Part{
		{Filename: "ticket-1.png", ContentType: "image/png", ContentID: "qr-1", Data: []byte("png")},
		{Filename: "tickets-TS-ABCDE.pdf", ContentType: "application/pdf", Data: []byte("<< /Type /Pages >> << /Type /Page >> << /Type/Page >>")}}})
	h := testRoutes(http.NotFoundHandler(), m, nil, "")
	if code, body := get(t, h, "/__test/last-email-summary?to=a@example.com"); code != 200 ||
		body != `{"subject":"Your tickets","inline":1,"attachments":["tickets-TS-ABCDE.pdf"],"pdf_pages":2}`+"\n" {
		t.Fatalf("got %d %q", code, body)
	}
	if code, _ := get(t, h, "/__test/last-email-summary?to=nobody@example.com"); code != 404 {
		t.Errorf("no mail: got %d, want 404", code)
	}
	if code, _ := get(t, h, "/__test/last-email-summary?to=a@example.com", "X-Forwarded-For", "203.0.113.9"); code != 404 {
		t.Errorf("proxied: got %d, want 404", code)
	}
}

func TestWorkerSendsQueuedEmail(t *testing.T) {
	m := &mailer.Memory{}
	w := newWorker(nil, m, config.Config{}, nil)
	h := w.Handlers["send_email"]
	if h == nil || w.Handlers["order_confirmation"] == nil || w.Handlers[sales.RefundJob] == nil || w.Parked[sales.RefundJob] == nil || w.Parked[sales.OrderConfirmationJob] == nil {
		t.Fatal("serve's worker is missing the send_email, order_confirmation or refund handler, or a parking hook")
	}
	if err := h(context.Background(), []byte(`{"to":"a@example.com","subject":"Hi","text":"Hello"}`)); err != nil || len(m.Sent) != 1 {
		t.Fatalf("err=%v sent=%d", err, len(m.Sent))
	}
}

// In test mode the worker mails through the memory /__test/last-email reads,
// so the end-to-end suite sees order emails too.
func TestTestModeWorkerMailIsVisible(t *testing.T) {
	h, w, err := newHandler(local("127.0.0.1:3001"), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Handlers["send_email"](context.Background(),
		[]byte(`{"to":"b@example.com","subject":"Tickets","text":"Here: http://localhost:3001/orders/TS-7K2QF?t=abc"}`)); err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, h, "/__test/last-email?to=b@example.com"); code != 200 || body != "http://localhost:3001/orders/TS-7K2QF?t=abc" {
		t.Fatalf("got %d %q, want the worker's link", code, body)
	}
}

// pay-latest signs what Stripe would send for the fake's newest intent and
// serves it through the API's own webhook route.
func TestPayLatestSignsTheNewestIntent(t *testing.T) {
	fake := payments.NewFake()
	if _, err := fake.CreatePaymentIntent(context.Background(), plugin.PaymentIntentInput{Account: "acct_fake1", AmountCents: 2600, IdempotencyKey: "pi-1"}); err != nil {
		t.Fatal(err)
	}
	pi, _ := fake.Latest()
	var got stripe.Event
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ev, err := webhook.ConstructEventWithOptions(body, r.Header.Get("Stripe-Signature"), "whsec_e2e", webhook.ConstructEventOptions{Tolerance: 5 * time.Minute})
		if err != nil || r.Method != "POST" || r.URL.Path != "/v1/webhooks/stripe" {
			http.Error(w, fmt.Sprint(err), 400)
			return
		}
		got = ev
	})
	h := testRoutes(api, &mailer.Memory{}, fake, "whsec_e2e")
	post := func(header ...string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/__test/stripe/pay-latest", nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, hdr := range []string{"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if code := post(hdr, "203.0.113.9"); code != 404 {
			t.Errorf("%s: got %d, want 404", hdr, code)
		}
	}
	if code := post(); code != 200 {
		t.Fatalf("pay-latest: %d, want 200", code)
	}
	var obj struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(got.Data.Raw, &obj)
	if got.Type != "payment_intent.succeeded" || got.Account != "acct_fake1" || obj.ID != pi.ID || obj.Amount != 2600 || obj.Currency != "usd" || obj.Status != "succeeded" {
		t.Fatalf("event = %+v %+v", got, obj)
	}
	if code := post(); code != 200 {
		t.Fatal("pay-latest twice")
	}
	// Before any payment there's nothing to pay.
	rec := httptest.NewRecorder()
	testRoutes(api, &mailer.Memory{}, payments.NewFake(), "whsec_e2e").ServeHTTP(rec, httptest.NewRequest("POST", "/__test/stripe/pay-latest", nil))
	if rec.Code != 404 {
		t.Fatalf("pay-latest with no payment: %d, want 404", rec.Code)
	}
}

// refund-latest signs the refund.updated Stripe would send for the fake's
// newest refund, with our metadata, through the API's own webhook route.
func TestRefundLatestSignsTheNewestRefund(t *testing.T) {
	ctx := context.Background()
	fake := payments.NewFake()
	pi, err := fake.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{Account: "acct_fake1", AmountCents: 2600, IdempotencyKey: "pi-1"})
	if err != nil {
		t.Fatal(err)
	}
	var got stripe.Event
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ev, err := webhook.ConstructEventWithOptions(body, r.Header.Get("Stripe-Signature"), "whsec_e2e", webhook.ConstructEventOptions{Tolerance: 5 * time.Minute})
		if err != nil || r.URL.Path != "/v1/webhooks/stripe" {
			http.Error(w, fmt.Sprint(err), 400)
			return
		}
		got = ev
	})
	h := testRoutes(api, &mailer.Memory{}, fake, "whsec_e2e")
	post := func(query string, header ...string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/__test/stripe/refund-latest"+query, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(""); code != 404 {
		t.Fatalf("refund-latest with no refund: %d, want 404", code)
	}
	md := map[string]string{"refund_id": "0199e5a0-0000-7000-8000-000000000001", "attempt": "0"}
	re, err := fake.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 2600, IdempotencyKey: "refund-x-0", Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	if code := post("", "X-Forwarded-For", "203.0.113.9"); code != 404 {
		t.Fatalf("through a proxy: %d, want 404", code)
	}
	for _, status := range []string{"", "failed"} {
		if code := post("?status=" + status); code != 200 {
			t.Fatalf("refund-latest %q: %d, want 200", status, code)
		}
		want := status
		if want == "" {
			want = "succeeded"
		}
		var obj struct {
			ID, Status    string
			PaymentIntent string `json:"payment_intent"`
			Amount        int64
			Metadata      map[string]string
		}
		_ = json.Unmarshal(got.Data.Raw, &obj)
		if got.Type != "refund.updated" || got.Account != "acct_fake1" || obj.ID != re.ID || obj.PaymentIntent != pi.ID ||
			obj.Amount != 2600 || obj.Status != want || obj.Metadata["refund_id"] != md["refund_id"] || obj.Metadata["attempt"] != "0" {
			t.Fatalf("event = %+v %+v", got, obj)
		}
	}
	if code := post("?status=pending"); code != 404 {
		t.Fatalf("status pending: %d, want 404", code)
	}
}

func TestTestModeRefusesLiveStripeKeys(t *testing.T) {
	for _, c := range []config.Config{
		{StripeSecretKey: "sk_live_abc", StripePublishableKey: "pk_live_abc", StripeWebhookSecret: "whsec_abc"},
		{StripeSecretKey: "rk_live_abc", StripePublishableKey: "pk_live_abc", StripeWebhookSecret: "whsec_abc"},
		{StripePublishableKey: "pk_live_abc"},
	} {
		cfg := local("127.0.0.1:3001")
		cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeWebhookSecret = c.StripeSecretKey, c.StripePublishableKey, c.StripeWebhookSecret
		if _, _, err := newHandler(cfg, nil, true); err == nil {
			t.Errorf("%+v: test mode started with a live key", c)
		} else if strings.Contains(err.Error(), "_abc") {
			t.Errorf("error %q repeats the key", err)
		}
		cfg.TurnstileSecret = "s"
		if _, _, err := newHandler(cfg, nil, false); err != nil {
			t.Errorf("%+v outside test mode: %v", c, err)
		}
	}
	cfg := local("127.0.0.1:3001")
	cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeWebhookSecret = "sk_test_abc", "pk_test_abc", "whsec_e2e"
	if _, _, err := newHandler(cfg, nil, true); err != nil {
		t.Fatalf("test keys in test mode: %v", err)
	}
}

func TestPaymentProvider(t *testing.T) {
	if p := paymentProvider(config.Config{}, false); p != nil {
		t.Fatalf("no key: %v, want none", p)
	}
	if p := paymentProvider(config.Config{StripeSecretKey: "sk_test_abc", StripePublishableKey: "pk_test_abc"}, false); p == nil || p.Name() != "stripe" || p.PublishableKey() != "pk_test_abc" {
		t.Fatalf("with a key: %v", p)
	}
	if p := paymentProvider(config.Config{}, true); p == nil || p.Name() != "test" {
		t.Fatalf("test mode: %v, want the fake", p)
	}
}

// Test mode with a test secret key takes real test-mode Stripe payments, for
// the manual smoke run; without one it keeps the fake. Live keys are refused
// (TestTestModeRefusesLiveStripeKeys).
func TestTestModeUsesStripeWithATestKey(t *testing.T) {
	cfg := local("127.0.0.1:3001")
	if st := paymentsStatus(t, cfg, true); st.Provider != "test" {
		t.Fatalf("no key: provider %q, want the fake", st.Provider)
	}
	cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeWebhookSecret = "sk_test_x", "pk_test_x", "whsec_x"
	if st := paymentsStatus(t, cfg, true); st.Provider != "stripe" {
		t.Fatalf("test key: provider %q, want stripe", st.Provider)
	}
	cfg.StripeSecretKey = "sk_live_x"
	if _, _, err := newHandler(cfg, nil, true); err == nil {
		t.Fatal("test mode started with a live key")
	}
}

// TS_PAYMENTS=direct outside test mode sells through the server's own Stripe
// account: the API says so, and the provider has nothing to connect.
func TestDirectModeServesTheServersOwnStripe(t *testing.T) {
	cfg := config.Config{HTTPAddr: "127.0.0.1:3000", TurnstileSecret: "s", PaymentsMode: "direct", Mail: "log",
		StripeSecretKey: "sk_test_abc", StripePublishableKey: "pk_test_abc", StripeWebhookSecret: "whsec_abc"}
	p := paymentProvider(cfg, false)
	if p == nil || p.Name() != "stripe" || p.PublishableKey() != "pk_test_abc" {
		t.Fatalf("provider %v", p)
	}
	if _, err := p.LinkURL("state", "https://x", ""); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("LinkURL: %v, want the direct provider's refusal", err)
	}
	_, w, err := newHandler(cfg, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if st := paymentsStatus(t, cfg, false); st.Mode != "direct" || st.Provider != "stripe" {
		t.Fatalf("payments status: %+v", st)
	}
	if w.Handlers[sales.RefundJob] == nil {
		t.Fatal("no refund handler")
	}
}

type paymentsStatusBody struct{ Mode, Provider string }

// paymentsStatus is GET /v1/organizers/acme/payments from a fresh organizer
// on a server built from cfg.
func paymentsStatus(t *testing.T, cfg config.Config, testMode bool) paymentsStatusBody {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	owner := testdb.Owner(t, pool)
	var user, org uuid.UUID
	if err := owner.QueryRow(ctx, `insert into users(email, name) values ('a@example.com', 'A') returning id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `insert into organizers(name, slug) values ('Acme', 'acme') returning id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'owner')`, org, user); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := db.WithOrganizer(ctx, pool, user, org, func(tx pgx.Tx, _ string) (err error) {
		_, key, err = apikeys.New(pool).Create(ctx, tx, org, user, "status", "read")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h, _, err := newHandler(cfg, pool, testMode)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, h, "/v1/organizers/acme/payments", "Authorization", "Bearer "+key)
	var st paymentsStatusBody
	if err := json.Unmarshal([]byte(body), &st); err != nil || code != 200 {
		t.Fatalf("payments status: %d %s", code, body)
	}
	return st
}

func TestServeUsesTheConfiguredMailer(t *testing.T) {
	m, err := newMailer(config.Config{Mail: "resend", ResendAPIKey: "re_abc", ResendAPIURL: "https://api.resend.com", MailFrom: "TicketSlinger <tickets@ticketslinger.com>"}, nil)
	if r, ok := m.(*mailer.Resend); err != nil || !ok || r.Key != "re_abc" || r.URL != "https://api.resend.com" || r.From != "TicketSlinger <tickets@ticketslinger.com>" {
		t.Fatalf("resend: %#v %v", m, err)
	}
	want := &mailer.SMTP{Host: "smtp.example.com", Port: 465, Username: "relay", Password: "pw", From: "a@example.com"}
	if m, err := newMailer(config.Config{Mail: "smtp", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUsername: "relay", SMTPPassword: "pw", MailFrom: "a@example.com"}, nil); err != nil || !reflect.DeepEqual(m, want) {
		t.Fatalf("smtp: %#v %v", m, err)
	}
	if m, err := newMailer(config.Config{Mail: "log"}, nil); err != nil || m != (mailer.Log{}) {
		t.Fatalf("log: %#v %v", m, err)
	}
	if _, err := newMailer(config.Config{Mail: "sendgrid"}, nil); err == nil {
		t.Fatal("an unknown mailer must be refused")
	}
}
