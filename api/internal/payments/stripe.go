// Package payments holds the payment providers: Stripe, and a fake for tests
// and the end-to-end suite.
package payments

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

// Stripe is Connect Standard: organizers own their accounts, and every
// charge is a direct charge on theirs.
type Stripe struct {
	c  *stripe.Client
	pk string
}

var _ plugin.PaymentProvider = (*Stripe)(nil)

// NewStripe talks to Stripe's API, or to backends when not nil (tests).
// Stripe's own logger stays off; our handlers log what matters.
func NewStripe(secretKey, publishableKey string, backends *stripe.Backends) *Stripe {
	if backends == nil {
		backends = stripe.NewBackendsWithConfig(&stripe.BackendConfig{
			HTTPClient:    &http.Client{Timeout: 15 * time.Second},
			LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
		})
	}
	return &Stripe{c: stripe.NewClient(secretKey, stripe.WithBackends(backends)), pk: publishableKey}
}

func (s *Stripe) Name() string           { return "stripe" }
func (s *Stripe) PublishableKey() string { return s.pk }

// CreateAccount makes a Standard connected account: the organizer gets the
// full Stripe dashboard, pays Stripe's fees and owns disputes with Stripe.
// Onboarding collects the email; a retry with the same key sends the same
// params, as Stripe's idempotency requires.
func (s *Stripe) CreateAccount(ctx context.Context, key string) (string, error) {
	p := &stripe.AccountCreateParams{
		Country: stripe.String("US"),
		Controller: &stripe.AccountCreateControllerParams{
			StripeDashboard:       &stripe.AccountCreateControllerStripeDashboardParams{Type: stripe.String("full")},
			Fees:                  &stripe.AccountCreateControllerFeesParams{Payer: stripe.String("account")},
			Losses:                &stripe.AccountCreateControllerLossesParams{Payments: stripe.String("stripe")},
			RequirementCollection: stripe.String("stripe"),
		},
	}
	p.SetIdempotencyKey(key)
	a, err := s.c.V1Accounts.Create(ctx, p)
	if err != nil {
		return "", fmt.Errorf("stripe: create account: %w", err)
	}
	return a.ID, nil
}

func (s *Stripe) OnboardingLink(ctx context.Context, account, refreshURL, returnURL string) (string, error) {
	l, err := s.c.V1AccountLinks.Create(ctx, &stripe.AccountLinkCreateParams{Account: stripe.String(account),
		RefreshURL: stripe.String(refreshURL), ReturnURL: stripe.String(returnURL), Type: stripe.String("account_onboarding")})
	if err != nil {
		return "", fmt.Errorf("stripe: account link: %w", err)
	}
	return l.URL, nil
}

func (s *Stripe) Account(ctx context.Context, account string) (plugin.AccountStatus, error) {
	a, err := s.c.V1Accounts.GetByID(ctx, account, nil)
	if err != nil {
		return plugin.AccountStatus{}, fmt.Errorf("stripe: account: %w", err)
	}
	return plugin.AccountStatus{ChargesEnabled: a.ChargesEnabled, DetailsSubmitted: a.DetailsSubmitted}, nil
}

// CreatePaymentIntent charges on the organizer's account (direct charge);
// our fee rides along as the application fee.
func (s *Stripe) CreatePaymentIntent(ctx context.Context, in plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	p := &stripe.PaymentIntentCreateParams{
		Amount:                  stripe.Int64(in.AmountCents),
		Currency:                stripe.String("usd"),
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)},
		Description:             stripe.String(in.Description),
	}
	if in.ApplicationFeeCents > 0 {
		p.ApplicationFeeAmount = stripe.Int64(in.ApplicationFeeCents)
	}
	for k, v := range in.Metadata {
		p.AddMetadata(k, v)
	}
	p.SetStripeAccount(in.Account)
	p.SetIdempotencyKey(in.IdempotencyKey)
	pi, err := s.c.V1PaymentIntents.Create(ctx, p)
	if err != nil {
		return plugin.PaymentIntent{}, fmt.Errorf("stripe: payment intent: %w", err)
	}
	return plugin.PaymentIntent{ID: pi.ID, ClientSecret: pi.ClientSecret, Account: in.Account, AmountCents: pi.Amount}, nil
}

func (s *Stripe) Refund(ctx context.Context, in plugin.RefundInput) (string, error) {
	p := &stripe.RefundCreateParams{PaymentIntent: stripe.String(in.PaymentIntentID), RefundApplicationFee: stripe.Bool(in.RefundApplicationFee)}
	if in.AmountCents > 0 {
		p.Amount = stripe.Int64(in.AmountCents)
	}
	p.SetStripeAccount(in.Account)
	p.SetIdempotencyKey(in.IdempotencyKey)
	r, err := s.c.V1Refunds.Create(ctx, p)
	if err != nil {
		return "", fmt.Errorf("stripe: refund: %w", err)
	}
	return r.ID, nil
}
