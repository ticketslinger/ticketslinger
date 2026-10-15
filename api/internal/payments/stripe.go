// Package payments holds the payment providers: Stripe, and a fake for tests
// and the end-to-end suite.
package payments

import (
	"context"
	"errors"
	"net"
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
		// account_invalid only: a bare 403 can be a restricted key without
		// Connect permissions, not a lost account. Never the message text.
		pe.NoAccess = pe.Refused && se.Code == stripe.ErrorCodeAccountInvalid
		return pe
	}
	// No answer: say roughly why, and nothing else (a URL can carry ids).
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		pe.Cause = "timeout"
	case errors.As(err, &ne):
		pe.Cause = "network"
	default:
		pe.Cause = "other"
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

// onAccount sends the call as the connected account; "" (direct mode) sends
// it as ourselves, with no Stripe-Account header at all.
func onAccount(p *stripe.Params, account string) {
	if account != "" {
		p.SetStripeAccount(account)
	}
}

// Refund gives back an exact amount. refund_application_fee stays unset:
// Stripe's flag refunds the fee pro rata, which is wrong for mixed-price
// orders, so RefundApplicationFee gives back the exact fee instead.
func (s *Stripe) Refund(ctx context.Context, in plugin.RefundInput) (plugin.Refund, error) {
	p := &stripe.RefundCreateParams{PaymentIntent: stripe.String(in.PaymentIntentID), Amount: stripe.Int64(in.AmountCents)}
	for k, v := range in.Metadata {
		p.AddMetadata(k, v)
	}
	onAccount(&p.Params, in.Account)
	p.SetIdempotencyKey(in.IdempotencyKey)
	r, err := s.c.V1Refunds.Create(ctx, p)
	var se *stripe.Error
	if errors.As(err, &se) && se.Code == stripe.ErrorCodeChargeAlreadyRefunded {
		return plugin.Refund{}, plugin.ErrAlreadyRefunded
	}
	if err != nil {
		return plugin.Refund{}, fail("refund", err)
	}
	return plugin.Refund{ID: r.ID, Status: string(r.Status), AmountCents: r.Amount, Metadata: r.Metadata}, nil
}

// ListRefunds reads GET /v1/refunds?payment_intent= on the connected
// account, every page.
func (s *Stripe) ListRefunds(ctx context.Context, account, intentID string) ([]plugin.Refund, error) {
	p := &stripe.RefundListParams{PaymentIntent: stripe.String(intentID)}
	p.Limit = stripe.Int64(100)
	if account != "" {
		p.SetStripeAccount(account)
	}
	var out []plugin.Refund
	for r, err := range s.c.V1Refunds.List(ctx, p).All(ctx) {
		if err != nil {
			return nil, fail("list refunds", err)
		}
		out = append(out, plugin.Refund{ID: r.ID, Status: string(r.Status), AmountCents: r.Amount, Metadata: r.Metadata})
	}
	return out, nil
}

// RefundApplicationFee refunds part or all of our fee on the intent's charge.
// The fee id is read on the connected account; the fee itself is ours, so the
// refund runs on the platform with no Stripe-Account header.
func (s *Stripe) RefundApplicationFee(ctx context.Context, in plugin.FeeRefundInput) (string, error) {
	g := &stripe.PaymentIntentRetrieveParams{}
	g.AddExpand("latest_charge")
	onAccount(&g.Params, in.Account)
	pi, err := s.c.V1PaymentIntents.Retrieve(ctx, in.PaymentIntentID, g)
	if err != nil {
		return "", fail("fee refund", err)
	}
	if pi.LatestCharge == nil || pi.LatestCharge.ApplicationFee == nil || pi.LatestCharge.ApplicationFee.ID == "" {
		return "", &plugin.ProviderError{Op: "stripe: fee refund", Refused: true, Code: "no_application_fee"}
	}
	feeID := pi.LatestCharge.ApplicationFee.ID
	p := &stripe.FeeRefundCreateParams{ID: stripe.String(feeID), Amount: stripe.Int64(in.AmountCents)}
	p.SetIdempotencyKey(in.IdempotencyKey)
	fr, err := s.c.V1FeeRefunds.Create(ctx, p)
	if err == nil {
		return fr.ID, nil
	}
	err = fail("fee refund", err)
	if !errors.Is(err, plugin.ErrRefused) {
		return "", err
	}
	// Stripe has no error code for a fee that's already refunded, so read
	// the fee (ours: on the platform) rather than guess from the refusal.
	// Done is all of it back, or enough back for this share with too little
	// left for it; anything else is the refusal as it came.
	fee, ferr := s.c.V1ApplicationFees.Retrieve(ctx, feeID, nil)
	if ferr == nil && (fee.Refunded || fee.AmountRefunded >= in.AmountCents && fee.Amount-fee.AmountRefunded < in.AmountCents) {
		return "", plugin.ErrAlreadyRefunded
	}
	return "", err
}

func (s *Stripe) PaymentIntentStatus(ctx context.Context, account, intentID string) (string, error) {
	p := &stripe.PaymentIntentRetrieveParams{}
	onAccount(&p.Params, account)
	pi, err := s.c.V1PaymentIntents.Retrieve(ctx, intentID, p)
	if err != nil {
		return "", fail("payment intent status", err)
	}
	return string(pi.Status), nil
}

func (s *Stripe) CancelPaymentIntent(ctx context.Context, account, intentID, key string) error {
	p := &stripe.PaymentIntentCancelParams{}
	onAccount(&p.Params, account)
	p.SetIdempotencyKey(key)
	if _, err := s.c.V1PaymentIntents.Cancel(ctx, intentID, p); err != nil {
		return fail("cancel payment intent", err)
	}
	return nil
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
