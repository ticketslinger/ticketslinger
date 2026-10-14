package payments_test

import (
	"context"
	"errors"
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
	acct, err := f.CreateAccount(ctx, "owner@example.com", "acct-1")
	if err != nil || acct != "acct_fake1" || f.Accounts() != 1 {
		t.Fatalf("account %q err %v count %d", acct, err, f.Accounts())
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
	if _, err := f.Account(ctx, "acct_nope"); err == nil {
		t.Fatal("unknown account answered")
	}
	pi, err := f.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{Account: acct, AmountCents: 5612, ApplicationFeeCents: 200, IdempotencyKey: "pi-1"})
	if err != nil || pi.ID != "pi_fake1" || !strings.HasPrefix(pi.ClientSecret, "pi_fake1_secret_") || pi.Account != acct || pi.AmountCents != 5612 {
		t.Fatalf("pi %+v err %v", pi, err)
	}
	if latest, ok := f.Latest(); !ok || latest != pi {
		t.Fatalf("latest %+v %v", latest, ok)
	}
	boom := errors.New("boom")
	f.FailNext(boom)
	if _, err := f.Refund(ctx, plugin.RefundInput{Account: acct, PaymentIntentID: pi.ID}); !errors.Is(err, boom) {
		t.Fatalf("FailNext: %v", err)
	}
	in := plugin.RefundInput{Account: acct, PaymentIntentID: pi.ID, RefundApplicationFee: true, IdempotencyKey: "refund-1"}
	if id, err := f.Refund(ctx, in); err != nil || id != "re_fake1" {
		t.Fatalf("refund %q %v", id, err)
	}
	if r := f.Refunds(); len(r) != 1 || r[0] != in {
		t.Fatalf("refunds %+v", r)
	}
}
