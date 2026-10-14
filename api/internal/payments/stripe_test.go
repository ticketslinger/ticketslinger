package payments_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

const secretKey = "sk_test_DoNotLeakThisKey123"

type request struct {
	method, path, account, idem, auth string
	form                              url.Values
}

// recorder stands in for api.stripe.com: it records each request and answers
// with the recorded response for its path, or with fail when set.
type recorder struct {
	mu     sync.Mutex
	reqs   []request
	status int
	fail   string
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = append(rec.reqs, request{r.Method, r.URL.Path, r.Header.Get("Stripe-Account"), r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"), r.Form})
	w.Header().Set("Content-Type", "application/json")
	if rec.fail != "" {
		w.WriteHeader(rec.status)
		_, _ = w.Write([]byte(rec.fail))
		return
	}
	file := map[string]string{
		"/v1/accounts":            "account.json",
		"/v1/accounts/acct_test1": "account.json",
		"/v1/account_links":       "account_link.json",
		"/v1/payment_intents":     "payment_intent.json",
		"/v1/refunds":             "refund.json",
	}[r.URL.Path]
	b, err := os.ReadFile("testdata/" + file)
	if file == "" || err != nil {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"no fixture"}}`))
		return
	}
	_, _ = w.Write(b)
}

func (rec *recorder) last() request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.reqs[len(rec.reqs)-1]
}

func newStripe(t *testing.T) (*payments.Stripe, *recorder) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	b := stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	return payments.NewStripe(secretKey, "pk_test_x", b), rec
}

func TestPaymentIntentIsADirectChargeWithOurFee(t *testing.T) {
	s, rec := newStripe(t)
	pi, err := s.CreatePaymentIntent(context.Background(), plugin.PaymentIntentInput{
		Account: "acct_test1", AmountCents: 5612, ApplicationFeeCents: 200, Description: "Gala: 2 tickets", IdempotencyKey: "pi-0190",
		Metadata: map[string]string{"order_id": "0190"},
	})
	if err != nil || pi.ID != "pi_test1" || pi.ClientSecret == "" || pi.Account != "acct_test1" || pi.AmountCents != 5612 {
		t.Fatalf("pi %+v err %v", pi, err)
	}
	r := rec.last()
	want := map[string]string{"amount": "5612", "currency": "usd", "application_fee_amount": "200", "automatic_payment_methods[enabled]": "true",
		"metadata[order_id]": "0190", "description": "Gala: 2 tickets"}
	for k, v := range want {
		if r.form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.form.Get(k), v)
		}
	}
	if r.method != "POST" || r.path != "/v1/payment_intents" || r.account != "acct_test1" || r.idem != "pi-0190" {
		t.Fatalf("%s %s account %q idempotency %q", r.method, r.path, r.account, r.idem)
	}
	if r.auth != "Bearer "+secretKey {
		t.Fatalf("authorization %q", r.auth)
	}
}

func TestNoFeeMeansNoApplicationFeeParam(t *testing.T) {
	s, rec := newStripe(t)
	if _, err := s.CreatePaymentIntent(context.Background(), plugin.PaymentIntentInput{Account: "acct_test1", AmountCents: 5612, IdempotencyKey: "pi-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.last().form["application_fee_amount"]; ok {
		t.Fatalf("application_fee_amount sent: %v", rec.last().form)
	}
}

func TestCreateAccountIsStandard(t *testing.T) {
	s, rec := newStripe(t)
	id, err := s.CreateAccount(context.Background(), "owner@example.com", "acct-0190")
	if err != nil || id != "acct_test1" {
		t.Fatalf("id %q err %v", id, err)
	}
	r := rec.last()
	want := map[string]string{
		"controller[stripe_dashboard][type]": "full", "controller[fees][payer]": "account", "controller[losses][payments]": "stripe",
		"controller[requirement_collection]": "stripe", "country": "US", "email": "owner@example.com",
	}
	for k, v := range want {
		if r.form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.form.Get(k), v)
		}
	}
	if _, ok := r.form["type"]; ok {
		t.Error("type sent: controller properties replace it")
	}
	if r.method != "POST" || r.path != "/v1/accounts" || r.idem != "acct-0190" || r.account != "" {
		t.Fatalf("%s %s idempotency %q account %q", r.method, r.path, r.idem, r.account)
	}
}

func TestOnboardingLink(t *testing.T) {
	s, rec := newStripe(t)
	u, err := s.OnboardingLink(context.Background(), "acct_test1", "https://app.example/refresh", "https://app.example/return")
	if err != nil || !strings.HasPrefix(u, "https://connect.stripe.com/") {
		t.Fatalf("url %q err %v", u, err)
	}
	r := rec.last()
	want := map[string]string{"account": "acct_test1", "type": "account_onboarding", "refresh_url": "https://app.example/refresh", "return_url": "https://app.example/return"}
	for k, v := range want {
		if r.form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.form.Get(k), v)
		}
	}
	if r.path != "/v1/account_links" || r.account != "" {
		t.Fatalf("path %s account %q", r.path, r.account)
	}
}

func TestAccountStatus(t *testing.T) {
	s, rec := newStripe(t)
	st, err := s.Account(context.Background(), "acct_test1")
	if err != nil || !st.ChargesEnabled || !st.DetailsSubmitted {
		t.Fatalf("status %+v err %v", st, err)
	}
	if r := rec.last(); r.method != "GET" || r.path != "/v1/accounts/acct_test1" {
		t.Fatalf("%s %s", r.method, r.path)
	}
}

func TestRefundReturnsTheFee(t *testing.T) {
	s, rec := newStripe(t)
	id, err := s.Refund(context.Background(), plugin.RefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", RefundApplicationFee: true, IdempotencyKey: "refund-0190"})
	if err != nil || id != "re_test1" {
		t.Fatalf("id %q err %v", id, err)
	}
	r := rec.last()
	if r.form.Get("refund_application_fee") != "true" || r.form.Get("payment_intent") != "pi_test1" || r.account != "acct_test1" || r.idem != "refund-0190" {
		t.Fatalf("form %v account %q idempotency %q", r.form, r.account, r.idem)
	}
	if _, ok := r.form["amount"]; ok {
		t.Fatal("amount 0 must refund the rest, not send amount")
	}
	if _, err := s.Refund(context.Background(), plugin.RefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 1000, IdempotencyKey: "refund-2"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last().form.Get("amount"); got != "1000" {
		t.Fatalf("partial amount %q", got)
	}
}

func TestStripeErrorsAreWrapped(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{402, `{"error":{"type":"card_error","code":"card_declined","decline_code":"generic_decline","message":"Your card was declined."}}`, "card_declined"},
		{500, `{"error":{"type":"api_error","code":"api_error_code","message":"Something went wrong on Stripe's end."}}`, "api_error_code"},
	} {
		s, rec := newStripe(t)
		rec.status, rec.fail = tc.status, tc.body
		_, err := s.CreatePaymentIntent(context.Background(), plugin.PaymentIntentInput{Account: "acct_test1", AmountCents: 5612, IdempotencyKey: "pi-1"})
		var se *stripe.Error
		if !errors.As(err, &se) || se.HTTPStatusCode != tc.status {
			t.Fatalf("%d: %v is not a wrapped *stripe.Error", tc.status, err)
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "stripe: payment intent: ") {
			t.Errorf("%d: %q doesn't name %s", tc.status, err, tc.want)
		}
		if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), "DoNotLeak") {
			t.Errorf("%d: error leaks the secret key: %q", tc.status, err)
		}
	}
}

func TestStripeName(t *testing.T) {
	s, _ := newStripe(t)
	if s.Name() != "stripe" || s.PublishableKey() != "pk_test_x" {
		t.Fatalf("%q %q", s.Name(), s.PublishableKey())
	}
}
