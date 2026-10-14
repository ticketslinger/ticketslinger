package payments_test

import (
	"context"
	"errors"
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
	rin := plugin.RefundInput{Account: acct, PaymentIntentID: pi.ID, RefundApplicationFee: true, IdempotencyKey: "refund-1"}
	id, err := f.Refund(ctx, rin)
	if err != nil || !strings.HasPrefix(id, "re_fake") {
		t.Fatalf("refund %q %v", id, err)
	}
	if again, _ := f.Refund(ctx, rin); again != id {
		t.Fatalf("same refund key: %q, want %q", again, id)
	}
	if r := f.Refunds(); len(r) != 1 || r[0] != rin {
		t.Fatalf("refunds %+v", r)
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
