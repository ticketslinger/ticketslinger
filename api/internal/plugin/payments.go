// Package plugin holds the seams the commercial build plugs into. The open
// build registers only open implementations.
package plugin

import (
	"context"
	"errors"
	"fmt"
)

// ErrAlreadyRefunded: the payment has no money left to refund. A refund
// retried after the provider's idempotency window ends here, and that is done.
var ErrAlreadyRefunded = errors.New("payments: already refunded")

// ErrLinkUnavailable: the platform has no OAuth client id, so organizers
// can't link a Stripe account they already have.
var ErrLinkUnavailable = errors.New("plugin: linking existing accounts isn't configured")

// A provider call failed in one of two ways. ErrUnavailable: we couldn't
// reach the provider, it failed on its side, or it asked us to slow down;
// the same call may work later, and may even have worked already. ErrRefused:
// the provider answered and said no; the same call will get the same no.
var (
	ErrUnavailable = errors.New("payments: provider unavailable")
	ErrRefused     = errors.New("payments: provider refused")
)

// ProviderError is a failed provider call, carrying only what is safe to
// log: never keys, client secrets, tokens or the provider's own message.
// errors.Is matches ErrRefused or ErrUnavailable.
type ProviderError struct {
	Op        string // "create account", "payment intent", ...
	Refused   bool
	Status    int // the provider's HTTP status, 0 when there was no answer
	Type      string
	Code      string
	RequestID string
}

func (e *ProviderError) Error() string {
	kind := "unavailable"
	if e.Refused {
		kind = "refused"
	}
	return fmt.Sprintf("%s: %s (status %d, type %q, code %q, request %q)", e.Op, kind, e.Status, e.Type, e.Code, e.RequestID)
}

func (e *ProviderError) Is(target error) bool {
	return target == ErrRefused && e.Refused || target == ErrUnavailable && !e.Refused
}

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
	// LinkURL is where an organizer goes to link a Stripe account they
	// already have. "" with ErrLinkUnavailable when the platform has no
	// OAuth client id.
	LinkURL(state, redirectURI, email string) (string, error)
	// CompleteLink trades the code Stripe sent back for the account id.
	CompleteLink(ctx context.Context, code string) (accountID string, err error)
	// Unlink removes our platform's access to the account.
	Unlink(ctx context.Context, accountID string) error
}
