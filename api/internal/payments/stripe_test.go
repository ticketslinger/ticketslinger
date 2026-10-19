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
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

const secretKey = "sk_test_DoNotLeakThisKey123"

type request struct {
	method, path, account, idem, auth string
	hasAccount                        bool // a Stripe-Account header at all, even an empty one
	form                              url.Values
}

// recorder stands in for api.stripe.com: it records each request and answers
// with the recorded response for its path, or with fail when set.
type recorder struct {
	mu     sync.Mutex
	reqs   []request
	status int
	fail   string
	only   string            // fail only this path ("" fails every one)
	bodies map[string]string // answers by path, ahead of the fixtures
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, has := r.Header["Stripe-Account"]
	rec.reqs = append(rec.reqs, request{r.Method, r.URL.Path, r.Header.Get("Stripe-Account"), r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"), has, r.Form})
	w.Header().Set("Content-Type", "application/json")
	if rec.fail != "" && (rec.only == "" || rec.only == r.URL.Path) {
		w.WriteHeader(rec.status)
		_, _ = w.Write([]byte(rec.fail))
		return
	}
	if b, ok := rec.bodies[r.URL.Path]; ok {
		_, _ = w.Write([]byte(b))
		return
	}
	file := map[string]string{
		"/v1/accounts":                           "account.json",
		"/v1/accounts/acct_test1":                "account.json",
		"/v1/account_links":                      "account_link.json",
		"/v1/payment_intents":                    "payment_intent.json",
		"/v1/refunds":                            "refund.json",
		"/v1/payment_intents/pi_nofee":           "payment_intent.json",
		"/v1/payment_intents/pi_test1":           "payment_intent_with_charge.json",
		"/v1/payment_intents/pi_test1/cancel":    "payment_intent_canceled.json",
		"/v1/application_fees/fee_test1/refunds": "fee_refund.json",
		"/oauth/token":                           "oauth_token.json",
		"/oauth/deauthorize":                     "deauthorize.json",
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

// newStripe points every backend, Connect's OAuth one included, at the recorder.
func newStripe(t *testing.T) (*payments.Stripe, *recorder) {
	return newStripeWithClient(t, "ca_test")
}

func newStripeWithClient(t *testing.T, clientID string) (*payments.Stripe, *recorder) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	b := stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	return payments.NewStripe(secretKey, "pk_test_x", clientID, b), rec
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
	id, err := s.CreateAccount(context.Background(), "acct-0190")
	if err != nil || id != "acct_test1" {
		t.Fatalf("id %q err %v", id, err)
	}
	r := rec.last()
	want := map[string]string{
		"controller[stripe_dashboard][type]": "full", "controller[fees][payer]": "account", "controller[losses][payments]": "stripe",
		"controller[requirement_collection]": "stripe", "country": "US",
	}
	for k, v := range want {
		if r.form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.form.Get(k), v)
		}
	}
	if _, ok := r.form["type"]; ok {
		t.Error("type sent: controller properties replace it")
	}
	if _, ok := r.form["email"]; ok {
		t.Error("email sent: a retry must send identical params, and onboarding asks for it")
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

func TestRefundIsExactAndLeavesTheFeeAlone(t *testing.T) {
	s, rec := newStripe(t)
	r, err := s.Refund(context.Background(), plugin.RefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 2600,
		IdempotencyKey: "refund-r1-0", Metadata: map[string]string{"refund_id": "r1"}})
	if err != nil || r.ID != "re_test1" || r.Status != "succeeded" {
		t.Fatalf("%+v %v", r, err)
	}
	got := rec.last()
	if got.account != "acct_test1" || got.idem != "refund-r1-0" || got.form.Get("amount") != "2600" || got.form.Get("payment_intent") != "pi_test1" ||
		got.form.Has("refund_application_fee") || got.form.Get("metadata[refund_id]") != "r1" {
		t.Fatalf("request %+v", got)
	}
}

func TestFeeRefundGoesToTheChargesApplicationFee(t *testing.T) {
	s, rec := newStripe(t)
	id, err := s.RefundApplicationFee(context.Background(), plugin.FeeRefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "appfee-r1-0"})
	if err != nil || id != "fr_test1" {
		t.Fatalf("%s %v", id, err)
	}
	read, refund := rec.reqs[0], rec.last()
	if read.path != "/v1/payment_intents/pi_test1" || read.account != "acct_test1" || read.form.Get("expand[0]") != "latest_charge" {
		t.Fatalf("read %+v", read)
	}
	// The fee lives on our platform account: no Stripe-Account header.
	if refund.path != "/v1/application_fees/fee_test1/refunds" || refund.account != "" || refund.idem != "appfee-r1-0" || refund.form.Get("amount") != "100" {
		t.Fatalf("refund %+v", refund)
	}
}

func TestFeeRefundWithNoFeeIsRefused(t *testing.T) {
	s, _ := newStripe(t)
	// pi_nofee is served with no latest_charge.
	_, err := s.RefundApplicationFee(context.Background(), plugin.FeeRefundInput{Account: "acct_test1", PaymentIntentID: "pi_nofee", AmountCents: 100, IdempotencyKey: "appfee-r2-0"})
	if !errors.Is(err, plugin.ErrRefused) {
		t.Fatalf("%v, want refused", err)
	}
	var pe *plugin.ProviderError
	if !errors.As(err, &pe) || pe.Code != "no_application_fee" {
		t.Fatalf("%+v", err)
	}
}

// Stripe has no error code for a fee that's already refunded, so a refused
// fee refund reads the fee itself, on the platform: refunded already is
// ErrAlreadyRefunded, anything else stays the refusal it was.
func TestRefusedFeeRefundReadsTheFee(t *testing.T) {
	for name, tc := range map[string]struct {
		fee  string
		done bool
	}{
		"fully refunded":       {`{"id": "fee_test1", "object": "application_fee", "amount": 200, "amount_refunded": 200, "refunded": true}`, true},
		"this share refunded":  {`{"id": "fee_test1", "object": "application_fee", "amount": 200, "amount_refunded": 150, "refunded": false}`, true},
		"room left":            {`{"id": "fee_test1", "object": "application_fee", "amount": 200, "amount_refunded": 0, "refunded": false}`, false},
		"not this one's share": {`{"id": "fee_test1", "object": "application_fee", "amount": 200, "amount_refunded": 50, "refunded": false}`, false},
		// Another refund's share went back, and this one's still fits: Stripe said no for some other reason.
		"another share refunded": {`{"id": "fee_test1", "object": "application_fee", "amount": 200, "amount_refunded": 100, "refunded": false}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, rec := newStripe(t)
			rec.status, rec.only = 400, "/v1/application_fees/fee_test1/refunds"
			rec.fail = `{"error":{"type":"invalid_request_error","code":"some_new_code","message":"no"}}`
			rec.bodies = map[string]string{"/v1/application_fees/fee_test1": tc.fee}
			_, err := s.RefundApplicationFee(context.Background(), plugin.FeeRefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1",
				AmountCents: 100, IdempotencyKey: "appfee-r1-0"})
			if got := errors.Is(err, plugin.ErrAlreadyRefunded); got != tc.done {
				t.Fatalf("%v: already refunded %v, want %v", err, got, tc.done)
			}
			var pe *plugin.ProviderError
			if !tc.done && (!errors.As(err, &pe) || !pe.Refused || pe.Code != "some_new_code") {
				t.Fatalf("%v, want the refusal as it was", err)
			}
			read := rec.last()
			if read.method != http.MethodGet || read.path != "/v1/application_fees/fee_test1" || read.account != "" {
				t.Fatalf("fee read %+v, want a GET on the platform", read)
			}
		})
	}
}

func TestIntentStatusAndCancel(t *testing.T) {
	s, rec := newStripe(t)
	if st, err := s.PaymentIntentStatus(context.Background(), "acct_test1", "pi_test1"); err != nil || st != "succeeded" {
		t.Fatalf("%s %v", st, err)
	}
	if err := s.CancelPaymentIntent(context.Background(), "acct_test1", "pi_test1", "cancel-o1"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got.path != "/v1/payment_intents/pi_test1/cancel" || got.account != "acct_test1" || got.idem != "cancel-o1" {
		t.Fatalf("%+v", got)
	}
}

// Direct mode has no connected account: no Stripe-Account header at all.
func TestNoAccountMeansNoHeader(t *testing.T) {
	s, rec := newStripe(t)
	if _, err := s.Refund(context.Background(), plugin.RefundInput{PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "refund-d-0"}); err != nil {
		t.Fatal(err)
	}
	if rec.last().account != "" {
		t.Fatalf("account %q", rec.last().account)
	}
}

// A refund retried after Stripe's 24-hour idempotency window finds the money
// already back with the buyer: that's done, not an error to retry forever.
func TestAlreadyRefundedIsItsOwnError(t *testing.T) {
	s, rec := newStripe(t)
	rec.status, rec.fail = 400, `{"error":{"type":"invalid_request_error","code":"charge_already_refunded","message":"Charge ch_1 has already been refunded."}}`
	if _, err := s.Refund(context.Background(), plugin.RefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "refund-1"}); !errors.Is(err, plugin.ErrAlreadyRefunded) {
		t.Fatalf("got %v, want ErrAlreadyRefunded", err)
	}
	rec.fail = `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such payment_intent"}}`
	if _, err := s.Refund(context.Background(), plugin.RefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "refund-1"}); err == nil || errors.Is(err, plugin.ErrAlreadyRefunded) {
		t.Fatalf("another error: %v", err)
	}
}

// Stripe's errors come back as *plugin.ProviderError, with only what's safe
// to log: a declined card's error, for one, carries the PaymentIntent and its
// client secret, and none of that may reach a log line.
func TestStripeErrorsAreWrapped(t *testing.T) {
	pi := `"payment_intent":{"id":"pi_1","client_secret":"pi_1_secret_DoNotLog"}`
	for _, tc := range []struct {
		status  int
		body    string
		code    string
		refused bool
	}{
		{402, `{"error":{"type":"card_error","code":"card_declined","decline_code":"generic_decline","message":"Your card was declined.",` + pi + `}}`, "card_declined", true},
		{400, `{"error":{"type":"invalid_request_error","message":"Stripe no longer recommends Accounts v1 for new Connect integrations"}}`, "", true},
		{401, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: sk_test_****Key1"}}`, "", true},
		{409, `{"error":{"type":"idempotency_error","code":"idempotency_key_in_use","message":"Another request with this key is running."}}`, "idempotency_key_in_use", false},
		{429, `{"error":{"type":"invalid_request_error","code":"rate_limit","message":"Too many requests."}}`, "rate_limit", false},
		{500, `{"error":{"type":"api_error","code":"api_error_code","message":"Something went wrong on Stripe's end.",` + pi + `}}`, "api_error_code", false},
		{502, `<html>bad gateway</html>`, "", false},
	} {
		s, rec := newStripe(t)
		rec.status, rec.fail = tc.status, tc.body
		_, err := s.CreatePaymentIntent(context.Background(), plugin.PaymentIntentInput{Account: "acct_test1", AmountCents: 5612, IdempotencyKey: "pi-1"})
		var pe *plugin.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("%d: %v is not a *plugin.ProviderError", tc.status, err)
		}
		if tc.status != 502 && (pe.Status != tc.status || pe.Code != tc.code) {
			t.Errorf("%d: got status %d code %q, want code %q", tc.status, pe.Status, pe.Code, tc.code)
		}
		if errors.Is(err, plugin.ErrRefused) != tc.refused || errors.Is(err, plugin.ErrUnavailable) == tc.refused {
			t.Errorf("%d: refused %v unavailable %v, want refused %v", tc.status, errors.Is(err, plugin.ErrRefused), errors.Is(err, plugin.ErrUnavailable), tc.refused)
		}
		if !strings.HasPrefix(err.Error(), "stripe: payment intent: ") {
			t.Errorf("%d: %q doesn't name the call", tc.status, err)
		}
		for _, secret := range []string{"DoNotLog", "secret", "DoNotLeak", "Key1", "Accounts v1"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%d: error carries %q: %q", tc.status, secret, err)
			}
		}
	}
}

// No answer at all may still have done the work: unavailable, never refused.
func TestNoAnswerIsUnavailable(t *testing.T) {
	b := stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String("http://127.0.0.1:1"), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	s := payments.NewStripe(secretKey, "pk_test_x", "ca_test", b)
	_, err := s.CreateAccount(context.Background(), "acct-1-0")
	var pe *plugin.ProviderError
	if !errors.Is(err, plugin.ErrUnavailable) || errors.Is(err, plugin.ErrRefused) || !errors.As(err, &pe) || pe.Cause != "network" {
		t.Fatalf("connection refused: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the cause carries the address: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }))
	t.Cleanup(slow.Close)
	b = stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String(slow.URL), MaxNetworkRetries: stripe.Int64(0),
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}, LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	_, err = payments.NewStripe(secretKey, "pk_test_x", "ca_test", b).CreateAccount(context.Background(), "acct-1-0")
	if !errors.As(err, &pe) || pe.Cause != "timeout" || errors.Is(err, plugin.ErrRefused) {
		t.Fatalf("timeout: %v", err)
	}
}

// Only account_invalid means "no access to the account". A bare 403 may be
// a restricted key lacking Connect permissions, so it stays a plain refusal.
func TestNoAccessIsItsOwnRefusal(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		noAccess bool
	}{
		{403, `{"error":{"type":"invalid_request_error","message":"no access"}}`, false}, // a restricted key without Connect read
		{403, `{"error":{"type":"invalid_request_error","code":"account_invalid","message":"x"}}`, true},
		{400, `{"error":{"type":"invalid_request_error","code":"account_invalid","message":"x"}}`, true},
		{404, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"does not have access to account"}}`, false},
		{500, `{"error":{"type":"api_error","code":"account_invalid","message":"x"}}`, false},
	} {
		s, rec := newStripe(t)
		rec.status, rec.fail = tc.status, tc.body
		_, err := s.Account(context.Background(), "acct_test1")
		if errors.Is(err, plugin.ErrNoAccess) != tc.noAccess || tc.status < 500 && !errors.Is(err, plugin.ErrRefused) {
			t.Errorf("%d %s: no access %v, want %v", tc.status, tc.body, errors.Is(err, plugin.ErrNoAccess), tc.noAccess)
		}
	}
}

func TestLinkURLAsksForReadWrite(t *testing.T) {
	s, rec := newStripe(t)
	u, err := s.LinkURL("st", "https://app.example/stripe/oauth/callback", "o@example.com")
	if err != nil || !strings.HasPrefix(u, "https://connect.stripe.com/oauth/authorize?") {
		t.Fatalf("url %q err %v", u, err)
	}
	q, _ := url.Parse(u)
	want := map[string]string{"client_id": "ca_test", "response_type": "code", "scope": "read_write", "state": "st",
		"redirect_uri": "https://app.example/stripe/oauth/callback", "stripe_user[email]": "o@example.com"}
	for k, v := range want {
		if got := q.Query().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(rec.reqs) != 0 {
		t.Fatalf("building the URL called Stripe: %v", rec.reqs)
	}
	if u, _ := s.LinkURL("st", "https://app.example/cb", ""); strings.Contains(u, "stripe_user") {
		t.Fatalf("no email, but %q prefills one", u)
	}
	off, _ := newStripeWithClient(t, "")
	if u, err := off.LinkURL("st", "https://app.example/cb", ""); u != "" || !errors.Is(err, plugin.ErrLinkUnavailable) {
		t.Fatalf("without a client id: %q %v", u, err)
	}
	if _, err := off.CompleteLink(context.Background(), "ac_1"); !errors.Is(err, plugin.ErrLinkUnavailable) {
		t.Fatalf("complete without a client id: %v", err)
	}
	if err := off.Unlink(context.Background(), "acct_linked1"); !errors.Is(err, plugin.ErrLinkUnavailable) {
		t.Fatalf("unlink without a client id: %v", err)
	}
}

func TestCompleteLinkReturnsTheAccount(t *testing.T) {
	s, rec := newStripe(t)
	id, err := s.CompleteLink(context.Background(), "ac_code1")
	if err != nil || id != "acct_linked1" {
		t.Fatalf("id %q err %v", id, err)
	}
	r := rec.last()
	if r.method != "POST" || r.path != "/oauth/token" || r.form.Get("grant_type") != "authorization_code" || r.form.Get("code") != "ac_code1" {
		t.Fatalf("%s %s %v", r.method, r.path, r.form)
	}
	// An exchange that fails is refused or unavailable like any other call,
	// and never repeats the code.
	rec.status, rec.fail = 400, `{"error":"invalid_grant","error_description":"Authorization code does not exist: ac_code1"}`
	_, err = s.CompleteLink(context.Background(), "ac_code1")
	var pe *plugin.ProviderError
	if !errors.As(err, &pe) || !pe.Refused || pe.Code != "invalid_grant" || strings.Contains(err.Error(), "ac_code1") {
		t.Fatalf("bad code: %v", err)
	}
}

func TestUnlinkDeauthorizes(t *testing.T) {
	s, rec := newStripe(t)
	if err := s.Unlink(context.Background(), "acct_linked1"); err != nil {
		t.Fatal(err)
	}
	r := rec.last()
	if r.method != "POST" || r.path != "/oauth/deauthorize" || r.form.Get("client_id") != "ca_test" || r.form.Get("stripe_user_id") != "acct_linked1" {
		t.Fatalf("%s %s %v", r.method, r.path, r.form)
	}
	rec.status, rec.fail = 500, `{"error":{"type":"api_error","message":"oops"}}`
	if err := s.Unlink(context.Background(), "acct_linked1"); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("Stripe down: %v", err)
	}
}

func TestStripeName(t *testing.T) {
	s, _ := newStripe(t)
	if s.Name() != "stripe" || s.PublishableKey() != "pk_test_x" {
		t.Fatalf("%q %q", s.Name(), s.PublishableKey())
	}
}

// ListRefunds asks the connected account for the intent's refunds and
// keeps each one's metadata.
func TestListRefundsReadsTheIntentsRefunds(t *testing.T) {
	s, rec := newStripe(t)
	rec.bodies = map[string]string{"/v1/refunds": `{"object":"list","url":"/v1/refunds","has_more":false,"data":[
		{"id":"re_2","object":"refund","status":"pending","amount":2600,"metadata":{"refund_id":"r1","attempt":"1"}},
		{"id":"re_1","object":"refund","status":"failed","metadata":{"refund_id":"r1","attempt":"0"}}]}`}
	list, err := s.ListRefunds(context.Background(), "acct_test1", "pi_test1")
	if err != nil || len(list) != 2 || list[0].ID != "re_2" || list[0].Status != "pending" || list[0].AmountCents != 2600 || list[0].Metadata["attempt"] != "1" ||
		list[1].Metadata["refund_id"] != "r1" {
		t.Fatalf("listed %+v %v", list, err)
	}
	r := rec.last()
	if r.method != "GET" || r.path != "/v1/refunds" || r.account != "acct_test1" || r.form.Get("payment_intent") != "pi_test1" {
		t.Fatalf("%s %s account %q form %v", r.method, r.path, r.account, r.form)
	}
	rec.status, rec.fail = 403, `{"error":{"type":"invalid_request_error","code":"account_invalid"}}`
	if _, err := s.ListRefunds(context.Background(), "acct_test1", "pi_test1"); !errors.Is(err, plugin.ErrNoAccess) {
		t.Fatalf("lost account: %v, want ErrNoAccess", err)
	}
}

// Direct mode: the server's own account takes every charge, so no call
// carries a Stripe-Account header and no charge carries our fee.
func TestDirectChargesTheServersOwnAccount(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	b := stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	s := payments.NewDirectStripe(secretKey, "pk_test_x", b)
	ctx := context.Background()
	for _, fee := range []int64{0, 200} {
		pi, err := s.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{AmountCents: 5612, ApplicationFeeCents: fee, IdempotencyKey: "pi-d"})
		if err != nil || pi.Account != "" {
			t.Fatalf("fee %d: pi %+v err %v", fee, pi, err)
		}
		if r := rec.last(); r.hasAccount || r.form.Has("application_fee_amount") || r.path != "/v1/payment_intents" {
			t.Fatalf("fee %d: %s account header %v %q, form %v", fee, r.path, r.hasAccount, r.account, r.form)
		}
	}
	if _, err := s.Refund(ctx, plugin.RefundInput{PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "refund-d-0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PaymentIntentStatus(ctx, "", "pi_test1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPaymentIntent(ctx, "", "pi_test1", "cancel-d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListRefunds(ctx, "", "pi_test1"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rec.reqs {
		if r.hasAccount {
			t.Fatalf("%s %s sent Stripe-Account %q", r.method, r.path, r.account)
		}
	}
	calls := len(rec.reqs)
	_, err := s.CreateAccount(ctx, "acct-1")
	if !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, err := s.OnboardingLink(ctx, "acct_test1", "r", "r"); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("OnboardingLink: %v", err)
	}
	if _, err := s.LinkURL("state", "https://x", ""); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("LinkURL: %v", err)
	}
	if _, err := s.CompleteLink(ctx, "code"); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("CompleteLink: %v", err)
	}
	if err := s.Unlink(ctx, "acct_test1"); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := s.RefundApplicationFee(ctx, plugin.FeeRefundInput{PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "appfee-d"}); !errors.Is(err, plugin.ErrDirectPayments) {
		t.Fatalf("RefundApplicationFee: %v", err)
	}
	if len(rec.reqs) != calls {
		t.Fatalf("%d Stripe calls for things direct mode doesn't do", len(rec.reqs)-calls)
	}
	// A fee taken on a connected account before the switch still goes back.
	if id, err := s.RefundApplicationFee(ctx, plugin.FeeRefundInput{Account: "acct_test1", PaymentIntentID: "pi_test1", AmountCents: 100, IdempotencyKey: "appfee-c"}); err != nil || id == "" {
		t.Fatalf("fee from before the switch: %q %v", id, err)
	}
}
