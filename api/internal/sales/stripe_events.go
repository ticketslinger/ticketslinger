package sales

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// HandleStripeEvent acts on one verified Stripe event, once. An event about
// a payment belongs to the organizer that took it (eventOrganizer); any
// other names its organizer by account. One for nobody we know is logged and
// ignored. Each event id is recorded in the same transaction as what it
// does, so a retry or a second delivery changes nothing. An error rolls
// both back, and the webhook's 5xx makes Stripe send it again.
func (s *Service) HandleStripeEvent(ctx context.Context, ev stripe.Event) error {
	org, err := s.eventOrganizer(ctx, ev)
	if err != nil {
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
		case stripe.EventTypeRefundCreated, stripe.EventTypeRefundUpdated, stripe.EventTypeRefundFailed:
			return s.refundUpdated(ctx, tx, *org, ev)
		case stripe.EventTypeChargeRefunded:
			return chargeRefunded(ctx, tx, ev)
		case stripe.EventTypeAccountUpdated:
			return accountUpdated(ctx, tx, *org, ev)
		case stripe.EventTypeAccountApplicationDeauthorized:
			return deauthorized(ctx, tx, *org, ev)
		}
		return nil // recorded, and nothing else to do
	})
}

// paymentOf is the PaymentIntent an event is about: the object itself for
// payment_intent.*, its payment_intent for charge.* and refund.*.
func paymentOf(ev stripe.Event) string {
	if ev.Data == nil {
		return ""
	}
	var o struct {
		ID            string `json:"id"`
		PaymentIntent string `json:"payment_intent"`
	}
	if json.Unmarshal(ev.Data.Raw, &o) != nil {
		return ""
	}
	switch t := string(ev.Type); {
	case strings.HasPrefix(t, "payment_intent."):
		return o.ID
	case strings.HasPrefix(t, "charge."), strings.HasPrefix(t, "refund."):
		return o.PaymentIntent
	}
	return ""
}

// eventOrganizer: a payment's events belong to the organizer that took it,
// even after its account moved to another organizer; anything else to
// whoever the account is live for (or was last).
func (s *Service) eventOrganizer(ctx context.Context, ev stripe.Event) (*uuid.UUID, error) {
	var org *uuid.UUID
	if pi := paymentOf(ev); pi != "" {
		if err := s.Pool.QueryRow(ctx, `select organizer_for_payment($1, $2)`, pi, ev.Account).Scan(&org); err != nil || org != nil {
			return org, err
		}
	}
	err := s.Pool.QueryRow(ctx, `select organizer_for_stripe_account($1)`, ev.Account).Scan(&org)
	return org, err
}

// refundUpdated applies a refund event to the refund it's about, and only
// when the refund is ours for this row: Stripe's id is the one stored, or
// none is stored yet and the metadata names the row and its current attempt
// (an earlier attempt's late "failed" must never fail the retry that
// replaced it). The intent, account and amount must be the row's too.
// Anything else, such as a refund made by hand in Stripe, is logged and
// left alone.
func (s *Service) refundUpdated(ctx context.Context, tx pgx.Tx, org uuid.UUID, ev stripe.Event) error {
	var r struct {
		ID            string            `json:"id"`
		Amount        int64             `json:"amount"`
		Status        string            `json:"status"`
		PaymentIntent string            `json:"payment_intent"`
		FailureReason string            `json:"failure_reason"`
		Metadata      map[string]string `json:"metadata"`
	}
	if ev.Data != nil {
		if err := json.Unmarshal(ev.Data.Raw, &r); err != nil {
			return err
		}
	}
	notOurs := func() error {
		slog.InfoContext(ctx, "refund event that isn't for a refund of ours as it stands; changing nothing",
			"event", ev.ID, "type", ev.Type, "refund", r.ID, "payment_intent", r.PaymentIntent, "account", ev.Account)
		return nil
	}
	if r.ID == "" || r.PaymentIntent == "" {
		return notOurs()
	}
	// The payment's order first, as every refund path takes it, so the match
	// below can't race a retry or the job moving the row on.
	var order uuid.UUID
	err := tx.QueryRow(ctx, `select o.id from orders o join payments p on p.order_id = o.id
		where p.payment_intent_id = $1 and p.stripe_account_id = $2 for update of o`, r.PaymentIntent, ev.Account).Scan(&order)
	var id uuid.UUID
	if err == nil {
		attempt, aerr := strconv.Atoi(r.Metadata["attempt"])
		err = tx.QueryRow(ctx, `select r.id from refunds r join payments p on p.id = r.payment_id
			where p.order_id = $1 and r.amount_cents = $2
			  and (r.stripe_refund_id = $3 or (r.stripe_refund_id is null and r.id::text = $4 and r.attempt = $5 and $6))
			for update of r`, order, r.Amount, r.ID, r.Metadata["refund_id"], attempt, aerr == nil).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return notOurs()
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update refunds set stripe_refund_id = $2, updated_at = statement_timestamp()
		where id = $1 and stripe_refund_id is null`, id, r.ID); err != nil {
		return err
	}
	return s.finishRefund(ctx, tx, org, id, r.Status, r.FailureReason)
}

// chargeRefunded only cross-checks: in this API version it names no refund,
// so it can't say which row it's about. Stripe having refunded more on the
// charge than all we asked for and haven't seen fail means money went back
// that we didn't send (a refund made by hand in Stripe, say): a WARN for a
// person to look at. Less is normal while our refunds are on their way.
func chargeRefunded(ctx context.Context, tx pgx.Tx, ev stripe.Event) error {
	var c struct {
		PaymentIntent  string `json:"payment_intent"`
		AmountRefunded int64  `json:"amount_refunded"`
	}
	if ev.Data != nil {
		if err := json.Unmarshal(ev.Data.Raw, &c); err != nil {
			return err
		}
	}
	var ours *int64
	err := tx.QueryRow(ctx, `select (select coalesce(sum(r.amount_cents), 0) from refunds r where r.payment_id = p.id and r.status <> 'failed')
		from payments p where p.payment_intent_id = $1 and p.stripe_account_id = $2`, c.PaymentIntent, ev.Account).Scan(&ours)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not a payment of ours
	}
	if err != nil {
		return err
	}
	if c.AmountRefunded > *ours {
		slog.WarnContext(ctx, "charge.refunded: Stripe refunded more than we asked for", "event", ev.ID, "payment_intent", c.PaymentIntent,
			"stripe_refunded_cents", c.AmountRefunded, "our_refunds_cents", *ours)
	}
	return nil
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
	// A disconnected account stays off until the organizer connects again.
	_, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2 and stripe_disconnected_at is null,
		stripe_details_submitted = $3 where id = $1`, org, a.ChargesEnabled, a.DetailsSubmitted)
	return err
}

// deauthorized: the account (or we, from the dashboard) took our platform
// off it. The organizer is disconnected as if they'd done it here; the id
// stays so payments already made still find their orders. Only the event's
// account and time are read: these events may come in an older API version.
// A deauthorize made before the account last went live here is about an
// older link and is ignored; that grants nothing, since Stripe itself checks
// our access on every call.
func deauthorized(ctx context.Context, tx pgx.Tx, org uuid.UUID, ev stripe.Event) error {
	var since *int64
	var disconnected bool
	err := tx.QueryRow(ctx, `select floor(extract(epoch from stripe_connected_at))::bigint, stripe_disconnected_at is not null
		from organizers where id = $1 and stripe_account_id = $2 for no key update`, org, ev.Account).Scan(&since, &disconnected)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && disconnected {
		return nil // not this organizer's account any more, or disconnected already
	}
	if err != nil {
		return err
	}
	if since != nil && ev.Created < *since {
		slog.WarnContext(ctx, "account.application.deauthorized from before the account was last connected; ignoring",
			"event", ev.ID, "account", ev.Account, "created", ev.Created, "connected", *since)
		return nil
	}
	if _, err := tx.Exec(ctx, `update organizers set stripe_disconnected_at = statement_timestamp(), stripe_charges_enabled = false where id = $1`, org); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "organizer.stripe_disconnected",
		EntityType: "organizer", EntityID: org, After: map[string]string{"stripe_account_id": ev.Account, "via": "stripe"}})
}
