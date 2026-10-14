// Package plugin holds the seams the commercial build plugs into. The open
// build registers only open implementations.
package plugin

import (
	"context"
	"errors"
)

// ErrAlreadyRefunded: the payment has no money left to refund. A refund
// retried after the provider's idempotency window ends here, and that is done.
var ErrAlreadyRefunded = errors.New("payments: already refunded")

type AccountStatus struct{ ChargesEnabled, DetailsSubmitted bool }

type PaymentIntentInput struct {
	Account                          string // connected account: the charge is the organizer's
	AmountCents, ApplicationFeeCents int64
	Description, IdempotencyKey      string
	Metadata                         map[string]string
}

type PaymentIntent struct {
	ID, ClientSecret, Account string
	AmountCents               int64
}

type RefundInput struct {
	Account, PaymentIntentID string
	AmountCents              int64 // 0 refunds whatever is left
	RefundApplicationFee     bool
	IdempotencyKey           string
}

// PaymentProvider takes payments on an organizer's own connected account.
type PaymentProvider interface {
	Name() string // "stripe" or "test"; stored in payments.provider
	PublishableKey() string
	CreateAccount(ctx context.Context, idempotencyKey string) (accountID string, err error)
	OnboardingLink(ctx context.Context, accountID, refreshURL, returnURL string) (url string, err error)
	Account(ctx context.Context, accountID string) (AccountStatus, error)
	CreatePaymentIntent(ctx context.Context, in PaymentIntentInput) (PaymentIntent, error)
	Refund(ctx context.Context, in RefundInput) (refundID string, err error)
}
