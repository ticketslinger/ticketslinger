package sales

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// HandleStripeEvent acts on one verified Stripe event, once. The account it
// came from names the organizer; an account we don't know is logged and
// ignored. Each event id is recorded in the same transaction as what it
// does, so a retry or a second delivery changes nothing. An error rolls
// both back, and the webhook's 5xx makes Stripe send it again.
func (s *Service) HandleStripeEvent(ctx context.Context, ev stripe.Event) error {
	var org *uuid.UUID
	if err := s.Pool.QueryRow(ctx, `select organizer_for_stripe_account($1)`, ev.Account).Scan(&org); err != nil {
		return err
	}
	if org == nil {
		slog.InfoContext(ctx, "stripe event for an account we don't know", "event", ev.ID, "type", ev.Type, "account", ev.Account)
		return nil
	}
	return system.Run(ctx, s.Pool, *org, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `insert into stripe_events (id, type) values ($1, $2) on conflict do nothing`, ev.ID, string(ev.Type))
		if err != nil || tag.RowsAffected() == 0 {
			return err // 0 rows: handled already
		}
		switch ev.Type {
		case stripe.EventTypePaymentIntentSucceeded:
			return s.paymentSucceeded(ctx, tx, *org, ev)
		case stripe.EventTypeAccountUpdated:
			return accountUpdated(ctx, tx, *org, ev)
		}
		return nil // recorded, and nothing else to do
	})
}

// paymentSucceeded confirms the order an intent paid for, but only when our
// payments row matches the event exactly: the same intent, made on the
// account the event came from, for the order's total, in dollars. Anything
// else is recorded and logged, and confirms nothing.
func (s *Service) paymentSucceeded(ctx context.Context, tx pgx.Tx, org uuid.UUID, ev stripe.Event) error {
	var pi struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
	}
	if ev.Data != nil {
		if err := json.Unmarshal(ev.Data.Raw, &pi); err != nil {
			return err
		}
	}
	var order uuid.UUID
	var account, currency string
	var total int64
	err := tx.QueryRow(ctx, `select o.id, p.stripe_account_id, o.total_cents, o.currency
		from payments p join orders o on o.id = p.order_id where p.payment_intent_id = $1`, pi.ID).Scan(&order, &account, &total, &currency)
	mismatch := ""
	switch {
	case errors.Is(err, pgx.ErrNoRows) || pi.ID == "":
		mismatch = "payment_intent_id"
	case err != nil:
		return err
	case account != ev.Account:
		mismatch = "account"
	case pi.Amount != total:
		mismatch = "amount"
	case pi.Currency != "usd" || currency != "USD":
		mismatch = "currency"
	case pi.Status != "succeeded":
		mismatch = "status"
	}
	if mismatch != "" {
		slog.WarnContext(ctx, "payment_intent.succeeded doesn't match our payment; confirming nothing",
			"event", ev.ID, "payment_intent", pi.ID, "account", ev.Account, "mismatch", mismatch)
		return nil
	}
	if _, err := s.confirmOrder(ctx, tx, org, order); err != nil {
		return err
	}
	// Paid or refunded later, the money moved.
	_, err = tx.Exec(ctx, `update payments set status = 'succeeded', updated_at = statement_timestamp() where order_id = $1 and status <> 'succeeded'`, order)
	return err
}

// accountUpdated keeps the organizer's charges and details flags as Stripe
// has them.
func accountUpdated(ctx context.Context, tx pgx.Tx, org uuid.UUID, ev stripe.Event) error {
	var a struct {
		ID               string `json:"id"`
		ChargesEnabled   bool   `json:"charges_enabled"`
		DetailsSubmitted bool   `json:"details_submitted"`
	}
	if ev.Data != nil {
		if err := json.Unmarshal(ev.Data.Raw, &a); err != nil {
			return err
		}
	}
	if a.ID != ev.Account {
		slog.WarnContext(ctx, "account.updated about another account; ignoring", "event", ev.ID, "account", ev.Account, "object", a.ID)
		return nil
	}
	_, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2, stripe_details_submitted = $3 where id = $1`,
		org, a.ChargesEnabled, a.DetailsSubmitted)
	return err
}
