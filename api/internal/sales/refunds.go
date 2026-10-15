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

// RefundJob works one refunds row: the buyer's money first, then, once
// Stripe says that succeeded and the organizer chose it, our fee.
const RefundJob = "refund"

// refundJob is the whole payload: two ids. Amounts, Stripe ids and the
// buyer's email are read from the rows when it runs.
type refundJob struct {
	Organizer uuid.UUID `json:"organizer_id"`
	Refund    uuid.UUID `json:"refund_id"`
}

// requestRefund writes a requested refund for payment and queues its job,
// in the caller's transaction: the organizer's own (ts_app) or the
// platform's (ts_system). actor is the user who asked, nil for the platform.
func requestRefund(ctx context.Context, tx pgx.Tx, org, payment uuid.UUID, amount, fee int64, feeBack bool, reason string, actor *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into refunds (organizer_id, payment_id, amount_cents, fee_refunded, fee_cents, status, reason, actor_user_id)
		values ($1, $2, $3, $4, $5, 'requested', $6, $7) returning id`, org, payment, amount, feeBack, fee, reason, actor).Scan(&id); err != nil {
		return id, err
	}
	return id, jobs.Enqueue(ctx, tx, RefundJob, refundJob{Organizer: org, Refund: id})
}

type refundRow struct {
	id, order             uuid.UUID
	status                string
	amount, fee           int64
	stripeID, feeRefundID *string
	attempt               int
	intent, account       string
}

func loadRefund(ctx context.Context, tx pgx.Tx, id uuid.UUID) (refundRow, error) {
	r := refundRow{id: id}
	err := tx.QueryRow(ctx, `select p.order_id, r.status, r.amount_cents, r.fee_cents, r.stripe_refund_id, r.fee_refund_id, r.attempt,
			p.payment_intent_id, p.stripe_account_id
		from refunds r join payments p on p.id = r.payment_id where r.id = $1`, id).
		Scan(&r.order, &r.status, &r.amount, &r.fee, &r.stripeID, &r.feeRefundID, &r.attempt, &r.intent, &r.account)
	return r, err
}

// Refund handles RefundJob. Each Stripe call runs outside any transaction,
// under the row's key for this attempt; whatever Stripe answered is stored
// before the next step, so a crash or a second run never pays twice.
func Refund(s *Service) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j refundJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		var r refundRow
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) (err error) { r, err = loadRefund(ctx, tx, j.Refund); return })
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if s.Payments == nil {
			return errors.New("sales: a refund is due but there's no payment provider")
		}
		switch {
		case r.status == "requested" && r.stripeID == nil:
			return s.sendRefund(ctx, j.Organizer, r)
		case r.status == "succeeded" && r.fee > 0 && r.feeRefundID == nil:
			return s.sendFeeRefund(ctx, j.Organizer, r)
		}
		return nil // waiting for Stripe's word, failed, or done
	}
}

func (s *Service) sendRefund(ctx context.Context, org uuid.UUID, r refundRow) error {
	out, err := s.Payments.Refund(ctx, plugin.RefundInput{Account: r.account, PaymentIntentID: r.intent, AmountCents: r.amount,
		IdempotencyKey: fmt.Sprintf("refund-%s-%d", r.id, r.attempt), Metadata: map[string]string{"refund_id": r.id.String()}})
	switch {
	case errors.Is(err, plugin.ErrAlreadyRefunded):
		out = plugin.Refund{Status: "succeeded"} // nothing left on the charge: what we asked for went out
	case errors.Is(err, plugin.ErrNoAccess):
		slog.ErrorContext(ctx, "refund: our platform can't reach the account any more", "organizer", org, "order", r.order, "refund", r.id, "err", err)
		return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error { return s.failRefund(ctx, tx, org, r.id, "no_access") })
	case errors.Is(err, plugin.ErrRefused):
		return errors.Join(err, s.nextAttempt(ctx, org, r))
	case err != nil:
		return err
	}
	return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		if out.ID != "" {
			if _, err := tx.Exec(ctx, `update refunds set stripe_refund_id = $2, updated_at = statement_timestamp()
				where id = $1 and stripe_refund_id is null`, r.id, out.ID); err != nil {
				return err
			}
		}
		return s.finishRefund(ctx, tx, org, r.id, out.Status)
	})
}

func (s *Service) sendFeeRefund(ctx context.Context, org uuid.UUID, r refundRow) error {
	id, err := s.Payments.RefundApplicationFee(ctx, plugin.FeeRefundInput{Account: r.account, PaymentIntentID: r.intent,
		AmountCents: r.fee, IdempotencyKey: fmt.Sprintf("appfee-%s-%d", r.id, r.attempt)})
	switch {
	case errors.Is(err, plugin.ErrAlreadyRefunded):
		// Past the idempotency window, and Stripe has it back already. Its
		// fee refund id is lost to us, so the row says so in its place.
		id = "already_refunded:" + r.id.String()
	case errors.Is(err, plugin.ErrRefused):
		return errors.Join(err, s.nextAttempt(ctx, org, r))
	case err != nil:
		return err
	}
	return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update refunds set fee_refund_id = $2, updated_at = statement_timestamp()
			where id = $1 and fee_refund_id is null`, r.id, id)
		return err
	})
}

// nextAttempt moves the row to its next key after Stripe refused outright.
func (s *Service) nextAttempt(ctx context.Context, org uuid.UUID, r refundRow) error {
	return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update refunds set attempt = attempt + 1 where id = $1 and attempt = $2`, r.id, r.attempt)
		return err
	})
}

// finishRefund applies Stripe's word on a refund. It locks the refund's
// order, then its tickets, then the refund (the order refunds always take
// them in). succeeded: the seats its tickets held go back, a late payment's
// order becomes refunded, the fee step is queued when one is due, and the
// buyer is told. failed or canceled: the refund is failed for the organizer
// to see and retry; seats already given back stay given back. Anything else
// (pending, requires_action) waits for the next word. A final row is left
// alone, so replays change nothing.
func (s *Service) finishRefund(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, stripeStatus string) error {
	var order uuid.UUID
	if err := tx.QueryRow(ctx, `select p.order_id from refunds r join payments p on p.id = r.payment_id where r.id = $1`, id).Scan(&order); err != nil {
		return err
	}
	var email, event, organizer string
	if err := tx.QueryRow(ctx, `select o.email, e.name, g.name from orders o join events e on e.id = o.event_id
		join organizers g on g.id = o.organizer_id where o.id = $1 for update of o`, order).Scan(&email, &event, &organizer); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select 1 from tickets where order_id = $1 and refund_id = $2 order by id for update`, order, id); err != nil {
		return err
	}
	var status, reason string
	var amount, fee int64
	var feeDone bool
	if err := tx.QueryRow(ctx, `select status, reason, amount_cents, fee_cents, fee_refund_id is not null from refunds where id = $1 for update`, id).
		Scan(&status, &reason, &amount, &fee, &feeDone); err != nil {
		return err
	}
	switch stripeStatus {
	case "succeeded":
		if status != "requested" {
			return nil
		}
		if _, err := tx.Exec(ctx, `update refunds set status = 'succeeded', updated_at = statement_timestamp() where id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update tickets set released_at = statement_timestamp() where refund_id = $1 and released_at is null`, id); err != nil {
			return err
		}
		if reason == "late_payment" {
			if _, err := tx.Exec(ctx, `update orders set status = 'refunded' where id = $1 and status = 'cancelled'`, order); err != nil {
				return err
			}
		}
		if fee > 0 && !feeDone {
			if err := jobs.Enqueue(ctx, tx, RefundJob, refundJob{Organizer: org, Refund: id}); err != nil {
				return err
			}
		}
		// No link in it: nothing in this job's payload is a key to anything.
		subject, text := refundEmail(reason, event, organizer, amount)
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": email, "subject": subject, "text": text})
	case "failed", "canceled":
		if status == "failed" {
			return nil
		}
		return s.failRefund(ctx, tx, org, id, "stripe_failed")
	}
	return nil
}

// failRefund marks a refund failed and says so in the organizer's audit
// log. The caller holds the order lock or needs none (the row alone).
func (s *Service) failRefund(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, failure string) error {
	tag, err := tx.Exec(ctx, `update refunds set status = 'failed', failure = $2, updated_at = statement_timestamp()
		where id = $1 and status <> 'failed'`, id, failure)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "refund.failed",
		EntityType: "refund", EntityID: id, After: map[string]string{"failure": failure}})
}

// RefundParked is the queue's word that a refund job ran out of tries. The
// buyer may still be owed money, so it can't stay quiet: an ERROR naming the
// organizer, order and refund (never the buyer), and the refund failed
// (gave_up) so the organizer sees it and can try again. A buyer refund that
// already succeeded only lost its fee step: logged, nothing marked. The
// queue parks only real failures, never a shutdown (that hands the job back),
// so this runs once per parked job.
func RefundParked(s *Service) func(ctx context.Context, payload []byte, cause string) {
	return func(ctx context.Context, payload []byte, cause string) {
		ctx = context.WithoutCancel(ctx)
		var j refundJob
		if err := json.Unmarshal(payload, &j); err != nil {
			slog.ErrorContext(ctx, "refund stuck: unreadable job", "err", err)
			return
		}
		var order uuid.UUID
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
			var status string
			if err := tx.QueryRow(ctx, `select r.status, p.order_id from refunds r join payments p on p.id = r.payment_id where r.id = $1`, j.Refund).
				Scan(&status, &order); err != nil {
				return err
			}
			if status != "requested" {
				return nil
			}
			return s.failRefund(ctx, tx, j.Organizer, j.Refund, "gave_up")
		})
		slog.ErrorContext(ctx, "refund stuck: nothing will retry it", "organizer", j.Organizer, "order", order, "refund", j.Refund, "err", cause)
		if err != nil {
			slog.ErrorContext(ctx, "refund stuck: not marked failed", "refund", j.Refund, "err", err)
		}
	}
}

// refundEmail is what the buyer reads once the money is on its way back.
func refundEmail(reason, event, organizer string, amount int64) (subject, text string) {
	subject = "Your refund for " + event
	switch reason {
	case "late_payment":
		text = "Sorry, we couldn't keep your tickets for " + event + ". By the time your payment reached us, they weren't available any more, " +
			"so we've refunded you in full: " + dollars(amount) + " back to your card. It usually shows up in 5 to 10 days."
	case "event_cancelled":
		text = event + " has been cancelled, and " + organizer + " has refunded you " + dollars(amount) +
			". It goes back to the card you paid with and usually shows up in 5 to 10 days."
	default:
		text = organizer + " has refunded " + dollars(amount) + " of your order for " + event +
			". It goes back to the card you paid with and usually shows up in 5 to 10 days. Any tickets it covered won't scan any more."
	}
	return subject, text
}

// dollars formats cents as $26.00.
func dollars(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
