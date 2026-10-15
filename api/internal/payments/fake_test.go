package payments_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := payments.NewFake()
	if f.Name() != "test" || !strings.HasPrefix(f.PublishableKey(), "pk_test_") {
		t.Fatalf("%q %q", f.Name(), f.PublishableKey())
	}
	if _, ok := f.Latest(); ok {
		t.Fatal("latest before any intent")
	}
	acct, err := f.CreateAccount(ctx, "acct-1")
	// The migration's CHECK: acct_ then letters and digits only.
	if err != nil || !regexp.MustCompile(`^acct_fake[A-Za-z0-9]+$`).MatchString(acct) || f.Accounts() != 1 {
		t.Fatalf("account %q err %v count %d", acct, err, f.Accounts())
	}
	// The same key is the same account, as with Stripe; another key is another.
	if again, _ := f.CreateAccount(ctx, "acct-1"); again != acct || f.Accounts() != 1 {
		t.Fatalf("same key: %q, %d accounts", again, f.Accounts())
	}
	if other, _ := f.CreateAccount(ctx, "acct-2"); other == acct || f.Accounts() != 2 {
		t.Fatalf("other key: %q, %d accounts", other, f.Accounts())
	}
	if st, err := f.Account(ctx, acct); err != nil || st.ChargesEnabled {
		t.Fatalf("before onboarding: %+v %v", st, err)
	}
	if u, err := f.OnboardingLink(ctx, acct, "http://x/refresh", "http://x/return"); err != nil || u != "http://x/return" {
		t.Fatalf("link %q %v", u, err)
	}
	if st, err := f.Account(ctx, acct); err != nil || !st.ChargesEnabled || !st.DetailsSubmitted {
		t.Fatalf("after onboarding: %+v %v", st, err)
	}
	// An acct_fake id from before a restart is still known.
	if st, err := payments.NewFake().Account(ctx, acct); err != nil || st.ChargesEnabled {
		t.Fatalf("fresh fake: %+v %v", st, err)
	}
	if _, err := f.Account(ctx, "acct_real1"); err == nil {
		t.Fatal("a non-fake account answered")
	}
	in := plugin.PaymentIntentInput{Account: acct, AmountCents: 5612, ApplicationFeeCents: 200, IdempotencyKey: "pi-1"}
	pi, err := f.CreatePaymentIntent(ctx, in)
	if err != nil || !strings.HasPrefix(pi.ID, "pi_fake") || !strings.HasPrefix(pi.ClientSecret, pi.ID+"_secret_") || pi.Account != acct || pi.AmountCents != 5612 {
		t.Fatalf("pi %+v err %v", pi, err)
	}
	if again, _ := f.CreatePaymentIntent(ctx, in); again != pi {
		t.Fatalf("same key: %+v, want %+v", again, pi)
	}
	if latest, ok := f.Latest(); !ok || latest != pi {
		t.Fatalf("latest %+v %v", latest, ok)
	}
	boom := errors.New("boom")
	f.FailNext(boom)
	if _, err := f.Refund(ctx, plugin.RefundInput{Account: acct, PaymentIntentID: pi.ID}); !errors.Is(err, boom) {
		t.Fatalf("FailNext: %v", err)
	}
	rin := plugin.RefundInput{Account: acct, PaymentIntentID: pi.ID, AmountCents: 100, IdempotencyKey: "refund-1"}
	r, err := f.Refund(ctx, rin)
	if err != nil || !strings.HasPrefix(r.ID, "re_fake") {
		t.Fatalf("refund %+v %v", r, err)
	}
	if again, _ := f.Refund(ctx, rin); again != r {
		t.Fatalf("same refund key: %+v, want %+v", again, r)
	}
	if got := f.Refunds(); len(got) != 1 || !reflect.DeepEqual(got[0], rin) {
		t.Fatalf("refunds %+v", got)
	}
}

func TestFakeLinks(t *testing.T) {
	f := payments.NewFake()
	ctx := context.Background()
	u, err := f.LinkURL("st-1", "http://localhost:3002/stripe/oauth/callback", "o@example.com")
	if err != nil || u != "http://localhost:3002/stripe/oauth/callback?code=fake_code_1&state=st-1" {
		t.Fatalf("url %q err %v", u, err)
	}
	id, err := f.CompleteLink(ctx, "fake_code_1")
	if err != nil || id != "acct_fakelink1" {
		t.Fatalf("id %q err %v", id, err)
	}
	if st, err := f.Account(ctx, id); err != nil || !st.ChargesEnabled {
		t.Fatalf("a linked account takes payments: %+v %v", st, err)
	}
	if _, err := f.CompleteLink(ctx, "ac_real"); !errors.Is(err, plugin.ErrRefused) {
		t.Fatalf("unknown code: %v", err)
	}
	f.LinkReturns("acct_fakelink9")
	if id, _ := f.CompleteLink(ctx, "fake_code_2"); id != "acct_fakelink9" {
		t.Fatalf("LinkReturns: %q", id)
	}
	f.FailNext(errors.New("down"))
	if err := f.Unlink(ctx, id); err == nil || len(f.Unlinked()) != 0 {
		t.Fatalf("failed unlink: %v %v", err, f.Unlinked())
	}
	if err := f.Unlink(ctx, id); err != nil || len(f.Unlinked()) != 1 || f.Unlinked()[0] != "acct_fakelink1" {
		t.Fatalf("unlink: %v %v", err, f.Unlinked())
	}
}

func TestFakeRefundsAreExactAndBounded(t *testing.T) {
	f := payments.NewFake()
	ctx := context.Background()
	pi, _ := f.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{Account: "acct_fake1", AmountCents: 5200, ApplicationFeeCents: 200, IdempotencyKey: "pi-1"})
	r1, err := f.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 2600, IdempotencyKey: "refund-a-0"})
	if err != nil || r1.Status != "succeeded" {
		t.Fatalf("%+v %v", r1, err)
	}
	if again, _ := f.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 2600, IdempotencyKey: "refund-a-0"}); again.ID != r1.ID {
		t.Fatal("same key, different refund")
	}
	if _, err := f.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 2601, IdempotencyKey: "refund-b-0"}); !errors.Is(err, plugin.ErrRefused) {
		t.Fatalf("over-refund: %v, want refused", err)
	}
	if _, err := f.RefundApplicationFee(ctx, plugin.FeeRefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 201, IdempotencyKey: "appfee-a-0"}); !errors.Is(err, plugin.ErrRefused) {
		t.Fatalf("over-refunded fee: %v, want refused", err)
	}
	if _, err := f.RefundApplicationFee(ctx, plugin.FeeRefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 100, IdempotencyKey: "appfee-a-0"}); err != nil {
		t.Fatal(err)
	}
	if amount, fee := f.Refunded(pi.ID); amount != 2600 || fee != 100 {
		t.Fatalf("refunded %d, fee %d", amount, fee)
	}
	f.RefundsPending(true)
	if r, _ := f.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, AmountCents: 100, IdempotencyKey: "refund-c-0"}); r.Status != "pending" {
		t.Fatalf("status %s, want pending", r.Status)
	}
	if got := f.FeeRefunds(); len(got) != 1 || got[0].AmountCents != 100 {
		t.Fatalf("fee refunds %+v", got)
	}
}

func TestFakeIntentStatusAndCancel(t *testing.T) {
	f := payments.NewFake()
	ctx := context.Background()
	pi, _ := f.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{Account: "acct_fake1", AmountCents: 2600, IdempotencyKey: "pi-2"})
	if st, _ := f.PaymentIntentStatus(ctx, "acct_fake1", pi.ID); st != "requires_payment_method" {
		t.Fatalf("status %s", st)
	}
	f.SetIntentStatus(pi.ID, "succeeded")
	err := f.CancelPaymentIntent(ctx, "acct_fake1", pi.ID, "cancel-x")
	var pe *plugin.ProviderError
	if !errors.Is(err, plugin.ErrRefused) || !errors.As(err, &pe) || pe.Code != "payment_intent_unexpected_state" {
		t.Fatalf("cancelling a succeeded intent: %v, want refused", err)
	}
	f.SetIntentStatus(pi.ID, "requires_action")
	if err := f.CancelPaymentIntent(ctx, "acct_fake1", pi.ID, "cancel-x"); err != nil || !f.Cancelled(pi.ID) {
		t.Fatalf("%v %v", err, f.Cancelled(pi.ID))
	}
	if err := f.CancelPaymentIntent(ctx, "acct_fake1", pi.ID, "cancel-x"); err != nil {
		t.Fatalf("cancelling twice: %v", err)
	}
}
