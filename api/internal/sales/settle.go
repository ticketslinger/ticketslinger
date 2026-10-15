package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// SettleOrderJob decides an order still pending an hour after its hold ended.
const SettleOrderJob = "settle_order"

// CancelPaymentJob stops the intent of an order still pending on a
// cancelled event, so nobody is charged for it.
const CancelPaymentJob = "cancel_payment"

// settleJob is the whole payload of both jobs.
type settleJob struct {
	Organizer uuid.UUID `json:"organizer_id"`
	Order     uuid.UUID `json:"order_id"`
}

// SettleOrder asks Stripe what became of an abandoned order's payment. Paid:
// confirmed, exactly as the webhook would (so a lost webhook can't leave a
// buyer charged with no order). Still processing: asked again later. Anything
// else: the intent is cancelled at Stripe, so it can never be charged, and the
// order cancelled here. An order with no payment at all is just cancelled.
// Organizers never saw the order, so nothing is audited.
func SettleOrder(s *Service) jobs.Handler { return s.settler(false) }

// CancelPayment handles CancelPaymentJob the same way, and audits the
// cancelled order as the system's: the organizer's cancellation caused it.
// A Pay storing its intent as the event is cancelled sees it and cancels its
// own intent (see Pay), so nothing slips between the two.
func CancelPayment(s *Service) jobs.Handler { return s.settler(true) }

func (s *Service) settler(audited bool) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j settleJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		var status string
		var intent, account *string
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select o.status, p.payment_intent_id, p.stripe_account_id
				from orders o left join payments p on p.order_id = o.id where o.id = $1`, j.Order).Scan(&status, &intent, &account)
		})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && status != "pending" {
			return nil
		}
		if err != nil {
			return err
		}
		if intent == nil {
			return s.abandon(ctx, j, audited)
		}
		if s.Payments == nil {
			return errors.New("sales: a payment needs settling but there's no payment provider")
		}
		// Stripe calls run outside any transaction.
		st, err := s.Payments.PaymentIntentStatus(ctx, *account, *intent)
		if err == nil && st != "succeeded" && st != "processing" && st != "canceled" {
			err = s.Payments.CancelPaymentIntent(ctx, *account, *intent, "cancel-"+j.Order.String())
			st = "canceled"
			// It moved on (paid, maybe, just now): read where it went.
			if pe := (*plugin.ProviderError)(nil); errors.As(err, &pe) && pe.Code == "payment_intent_unexpected_state" {
				st, err = s.Payments.PaymentIntentStatus(ctx, *account, *intent)
			}
		}
		if errors.Is(err, plugin.ErrRefused) {
			// It will say the same again (no access to the account, say). Stop
			// asking: a payment that lands anyway is refunded by the webhook.
			slog.WarnContext(ctx, "stripe refused to settle a payment; leaving it", "order", j.Order, "intent", *intent, "err", err)
			return nil
		}
		if err != nil {
			return err
		}
		switch st {
		case "succeeded":
			return system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error { return s.markPaid(ctx, tx, j.Organizer, j.Order) })
		case "canceled":
			return s.abandon(ctx, j, audited)
		}
		return fmt.Errorf("sales: payment %s, asking again later", st)
	}
}

// abandon cancels a pending order that took no money, taking the checkout's
// lock and then the order's, as Pay and confirmOrder do. Its payment is
// cancelled too, and its client secret dropped, so the checkout page says
// what happened.
func (s *Service) abandon(ctx context.Context, j settleJob, audited bool) error {
	return system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select 1 from checkouts where id = (select checkout_id from orders where id = $1) for update`, j.Order); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update orders set status = 'cancelled' where id = $1 and status = 'pending'`, j.Order)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `update payments set status = 'cancelled', client_secret = '', updated_at = statement_timestamp() where order_id = $1`, j.Order); err != nil {
			return err
		}
		if !audited {
			return nil
		}
		return audit.Write(ctx, tx, audit.Entry{Organizer: j.Organizer, Actor: audit.Actor{System: true}, Action: "order.cancelled",
			EntityType: "order", EntityID: j.Order, Before: map[string]string{"status": "pending"}, After: map[string]string{"status": "cancelled"}})
	})
}
