// Package payments holds the payment providers: Stripe, and a fake for tests
// and the end-to-end suite.
package payments

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

// Stripe is Connect Standard: organizers own their accounts, and every
// charge is a direct charge on theirs.
type Stripe struct {
	c        *stripe.Client
	pk       string
	clientID string // Connect OAuth (ca_...); "" turns linking off
}

var _ plugin.PaymentProvider = (*Stripe)(nil)

// NewStripe talks to Stripe's API, or to backends when not nil (tests).
// Stripe's own logger stays off; our handlers log what matters.
func NewStripe(secretKey, publishableKey, clientID string, backends *stripe.Backends) *Stripe {
	if backends == nil {
		backends = stripe.NewBackendsWithConfig(&stripe.BackendConfig{
			HTTPClient:    &http.Client{Timeout: 15 * time.Second},
			LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
		})
	}
	return &Stripe{c: stripe.NewClient(secretKey, stripe.WithBackends(backends)), pk: publishableKey, clientID: clientID}
}

// fail turns a Stripe error into a *plugin.ProviderError that keeps only
// what is safe to log. Stripe's own error can carry the PaymentIntent, client
// secret and all, so it never leaves this package. A 4xx is a refusal, except
// 409 (a request with the same idempotency key is still running) and 429
// (slow down): both may work if tried again. No answer, or a 5xx, may even
// have done the work, so the caller retries with the same idempotency key.
func fail(op string, err error) error {
	pe := &plugin.ProviderError{Op: "stripe: " + op}
	var se *stripe.Error
	if errors.As(err, &se) {
		pe.Status, pe.Type, pe.Code, pe.RequestID = se.HTTPStatusCode, string(se.Type), string(se.Code), se.RequestID
		if pe.Code == "" {
			pe.Code = se.OAuthError // OAuth errors say what went wrong here, like invalid_grant
		}
		st := se.HTTPStatusCode
		pe.Refused = st >= 400 && st < 500 && st != http.StatusConflict && st != http.StatusTooManyRequests
	}
	return pe
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
		return "", fail("create account", err)
	}
	return a.ID, nil
}

func (s *Stripe) OnboardingLink(ctx context.Context, account, refreshURL, returnURL string) (string, error) {
	l, err := s.c.V1AccountLinks.Create(ctx, &stripe.AccountLinkCreateParams{Account: stripe.String(account),
		RefreshURL: stripe.String(refreshURL), ReturnURL: stripe.String(returnURL), Type: stripe.String("account_onboarding")})
	if err != nil {
		return "", fail("account link", err)
	}
	return l.URL, nil
}

func (s *Stripe) Account(ctx context.Context, account string) (plugin.AccountStatus, error) {
	a, err := s.c.V1Accounts.GetByID(ctx, account, nil)
	if err != nil {
		return plugin.AccountStatus{}, fail("account", err)
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
		return plugin.PaymentIntent{}, fail("payment intent", err)
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
	var se *stripe.Error
	if errors.As(err, &se) && se.Code == stripe.ErrorCodeChargeAlreadyRefunded {
		return "", plugin.ErrAlreadyRefunded
	}
	if err != nil {
		return "", fail("refund", err)
	}
	return r.ID, nil
}

// LinkURL is Stripe's OAuth page for Standard accounts, asking for
// read_write so we can charge on the account as with one we created.
func (s *Stripe) LinkURL(state, redirectURI, email string) (string, error) {
	if s.clientID == "" {
		return "", plugin.ErrLinkUnavailable
	}
	p := &stripe.AuthorizeURLParams{
		ClientID: stripe.String(s.clientID), ResponseType: stripe.String("code"), Scope: stripe.String("read_write"),
		State: stripe.String(state), RedirectURI: stripe.String(redirectURI),
	}
	if email != "" {
		p.StripeUser = &stripe.OAuthStripeUserParams{Email: stripe.String(email)}
	}
	return s.c.OAuth.AuthorizeURL(p), nil
}

// CompleteLink trades the code for the account id. The access and refresh
// tokens in Stripe's answer are dropped here: charges use our own key and the
// Stripe-Account header.
func (s *Stripe) CompleteLink(ctx context.Context, code string) (string, error) {
	if s.clientID == "" {
		return "", plugin.ErrLinkUnavailable
	}
	t, err := s.c.OAuth.Create(ctx, &stripe.OAuthTokenParams{GrantType: stripe.String("authorization_code"), Code: stripe.String(code)})
	if err != nil {
		return "", fail("oauth token", err)
	}
	return t.StripeUserID, nil
}

// Unlink deauthorizes our platform on the account.
func (s *Stripe) Unlink(ctx context.Context, account string) error {
	if s.clientID == "" {
		return plugin.ErrLinkUnavailable
	}
	if _, err := s.c.OAuth.Delete(ctx, &stripe.DeauthorizeParams{ClientID: stripe.String(s.clientID), StripeUserID: stripe.String(account)}); err != nil {
		return fail("deauthorize", err)
	}
	return nil
}
