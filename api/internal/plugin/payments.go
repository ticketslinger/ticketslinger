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
	NoAccess  bool   // refused because our platform can't reach the account at all
	Status    int    // the provider's HTTP status, 0 when there was no answer
	Cause     string // with no answer: "timeout", "network" or "other"
	Type      string
	Code      string
	RequestID string
}

// ErrNoAccess: the provider refused because our platform has no access to
// the account (it was unlinked, or never ours). errors.Is also matches
// ErrRefused for these.
var ErrNoAccess = errors.New("payments: no access to the account")

func (e *ProviderError) Error() string {
	if e.Status == 0 && e.Cause != "" {
		return fmt.Sprintf("%s: unavailable (no answer: %s)", e.Op, e.Cause)
	}
	kind := "unavailable"
	if e.Refused {
		kind = "refused"
	}
	return fmt.Sprintf("%s: %s (status %d, type %q, code %q, request %q)", e.Op, kind, e.Status, e.Type, e.Code, e.RequestID)
}

func (e *ProviderError) Is(target error) bool {
	return target == ErrRefused && e.Refused || target == ErrUnavailable && !e.Refused || target == ErrNoAccess && e.NoAccess
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
	Account, PaymentIntentID string // Account "" means no Stripe-Account header (direct mode)
	AmountCents              int64  // exact, > 0
	IdempotencyKey           string
	Metadata                 map[string]string // refund_id: our refunds.id, so its webhooks find the row
}

// Refund is the provider's answer: Status is its word for it (pending,
// requires_action, succeeded, failed, canceled).
type Refund struct{ ID, Status string }

type FeeRefundInput struct {
	Account, PaymentIntentID string
	AmountCents              int64 // exact, > 0
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
	Refund(ctx context.Context, in RefundInput) (Refund, error)
	// RefundApplicationFee gives back part or all of our fee on the intent's charge.
	RefundApplicationFee(ctx context.Context, in FeeRefundInput) (feeRefundID string, err error)
	// PaymentIntentStatus is the intent's status as the provider has it now.
	PaymentIntentStatus(ctx context.Context, account, intentID string) (string, error)
	// CancelPaymentIntent stops an intent nobody will pay. A refusal with code
	// payment_intent_unexpected_state means it moved on (it may have succeeded).
	CancelPaymentIntent(ctx context.Context, account, intentID, idempotencyKey string) error
	// LinkURL is where an organizer goes to link a Stripe account they
	// already have. "" with ErrLinkUnavailable when the platform has no
	// OAuth client id.
	LinkURL(state, redirectURI, email string) (string, error)
	// CompleteLink trades the code Stripe sent back for the account id.
	CompleteLink(ctx context.Context, code string) (accountID string, err error)
	// Unlink removes our platform's access to the account.
	Unlink(ctx context.Context, accountID string) error
}
