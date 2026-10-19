package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

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
	id, order       uuid.UUID
	status          string
	amount, fee     int64
	stripeID        *string
	sent            bool // asked Stripe before (sent_at)
	feeDone         bool
	feeTaken        bool // the payment carried our fee; a direct-mode one never does
	attempt         int
	intent, account string
}

func loadRefund(ctx context.Context, tx pgx.Tx, id uuid.UUID) (refundRow, error) {
	r := refundRow{id: id}
	err := tx.QueryRow(ctx, `select p.order_id, r.status, r.amount_cents, r.fee_cents, r.stripe_refund_id, r.sent_at is not null,
			r.fee_refunded_at is not null, r.attempt, p.payment_intent_id, p.stripe_account_id, p.application_fee_cents > 0
		from refunds r join payments p on p.id = r.payment_id where r.id = $1`, id).
		Scan(&r.order, &r.status, &r.amount, &r.fee, &r.stripeID, &r.sent, &r.feeDone, &r.attempt, &r.intent, &r.account, &r.feeTaken)
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
		case r.status == "succeeded" && r.fee > 0 && r.feeTaken && !r.feeDone:
			return s.sendFeeRefund(ctx, j.Organizer, r)
		}
		return nil // waiting for Stripe's word, failed, done, or no fee was taken to give back
	}
}

// sendRefund asks Stripe for the row's money under its key for this
// attempt. A row asked for before, with no Stripe id stored, may already be
// at Stripe (the answer was lost) after Stripe has forgotten the key (24
// hours), when the same key would make a second refund. So it looks first,
// and a refund carrying this row's id and attempt is taken as the answer.
func (s *Service) sendRefund(ctx context.Context, org uuid.UUID, r refundRow) error {
	var out plugin.Refund
	if r.sent {
		found, err := s.findRefund(ctx, r)
		if errors.Is(err, plugin.ErrNoAccess) {
			slog.ErrorContext(ctx, "refund: our platform can't reach the account any more", "organizer", org, "order", r.order, "refund", r.id, "err", err)
			return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error { return s.failRefund(ctx, tx, org, r.id, "no_access", "") })
		}
		if err != nil {
			return err // never a reason to move to the next key
		}
		out = found
	} else if err := system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update refunds set sent_at = statement_timestamp() where id = $1 and sent_at is null`, r.id)
		return err
	}); err != nil {
		return err
	}
	var err error
	if out.ID == "" {
		out, err = s.Payments.Refund(ctx, plugin.RefundInput{Account: r.account, PaymentIntentID: r.intent, AmountCents: r.amount,
			IdempotencyKey: fmt.Sprintf("refund-%s-%d", r.id, r.attempt),
			Metadata:       map[string]string{"refund_id": r.id.String(), "attempt": strconv.Itoa(r.attempt)}})
	}
	switch {
	case errors.Is(err, plugin.ErrAlreadyRefunded):
		// The charge is empty: nothing is left on it to refund. That doesn't
		// prove this row's amount went out (another refund may have taken
		// it), but no retry of ours can send anything now, so it's final.
		out = plugin.Refund{Status: "succeeded"}
	case errors.Is(err, plugin.ErrNoAccess):
		slog.ErrorContext(ctx, "refund: our platform can't reach the account any more", "organizer", org, "order", r.order, "refund", r.id, "err", err)
		return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error { return s.failRefund(ctx, tx, org, r.id, "no_access", "") })
	case errors.Is(err, plugin.ErrRefused):
		return errors.Join(err, s.nextAttempt(ctx, org, r))
	case err != nil:
		return err
	}
	var stripeID *string
	if out.ID != "" {
		stripeID = &out.ID
	}
	return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		// The order first, as every refund path takes it, then the row, and
		// only if it's still on the attempt this answer is for.
		if _, err := tx.Exec(ctx, `select 1 from orders where id = $1 for update`, r.order); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update refunds set stripe_refund_id = coalesce($2, stripe_refund_id), updated_at = statement_timestamp()
			where id = $1 and attempt = $3 and ($2::text is null or stripe_refund_id is null or stripe_refund_id = $2)`, r.id, stripeID, r.attempt)
		if err != nil || tag.RowsAffected() == 0 {
			return err // 0 rows: the row moved on, so this answer is stale
		}
		return s.finishRefund(ctx, tx, org, r.id, out.Status, "")
	})
}

// findRefund is the refund Stripe made for this row's current attempt, if
// any: its metadata names the row and the attempt, and its amount is the
// row's. A refund of an earlier attempt that Stripe failed doesn't count
// (that's why the row moved on), and neither does one for another amount:
// metadata alone could be copied onto any refund.
func (s *Service) findRefund(ctx context.Context, r refundRow) (plugin.Refund, error) {
	list, err := s.Payments.ListRefunds(ctx, r.account, r.intent)
	if err != nil {
		return plugin.Refund{}, err
	}
	for _, x := range list {
		if x.Metadata["refund_id"] == r.id.String() && x.Metadata["attempt"] == strconv.Itoa(r.attempt) && x.AmountCents == r.amount {
			return x, nil
		}
	}
	return plugin.Refund{}, nil
}

func (s *Service) sendFeeRefund(ctx context.Context, org uuid.UUID, r refundRow) error {
	id, err := s.Payments.RefundApplicationFee(ctx, plugin.FeeRefundInput{Account: r.account, PaymentIntentID: r.intent,
		AmountCents: r.fee, IdempotencyKey: fmt.Sprintf("appfee-%s-%d", r.id, r.attempt)})
	var feeRefund *string // Stripe's id; unknown past its idempotency window
	switch {
	case errors.Is(err, plugin.ErrAlreadyRefunded):
	case errors.Is(err, plugin.ErrNoAccess):
		// The account is gone: no retry can reach it, so stop here.
		slog.ErrorContext(ctx, "refund: our fee can't go back, the account is gone", "organizer", org, "order", r.order, "refund", r.id, "err", err)
		return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error { return feeStuck(ctx, tx, org, r) })
	case errors.Is(err, plugin.ErrRefused):
		return errors.Join(err, s.nextAttempt(ctx, org, r))
	case err != nil:
		return err
	default:
		feeRefund = &id
	}
	return system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update refunds set fee_refund_id = $2, fee_refunded_at = statement_timestamp(), updated_at = statement_timestamp()
			where id = $1 and fee_refunded_at is null`, r.id, feeRefund)
		return err
	})
}

// feeStuck records that the buyer has their money but our fee didn't go
// back to the organizer, and nothing will retry it. The refund keeps its
// status. It writes at most one row per refund (the refund's row lock makes
// two at once take turns), and none while another job for the refund is
// still due to try the fee again.
func feeStuck(ctx context.Context, tx pgx.Tx, org uuid.UUID, r refundRow) error {
	if _, err := tx.Exec(ctx, `select 1 from refunds where id = $1 for update`, r.id); err != nil {
		return err
	}
	// A statement of its own, so it sees what a turn before ours wrote.
	var due bool
	if err := tx.QueryRow(ctx, `select refund_fee_stuck_due($1)`, r.id).Scan(&due); err != nil || !due {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "refund.fee_stuck",
		EntityType: "refund", EntityID: r.id, After: map[string]int64{"amount_cents": r.amount, "fee_cents": r.fee}})
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
// buyer is told, even when we had given up asking (gave_up) and the money
// went out after all. failed or canceled: the refund is failed for the
// organizer to see and retry; seats already given back stay given back. Anything else
// (pending, requires_action) waits for the next word. A final row is left
// alone, so replays change nothing.
//
// Stripe's refund webhooks (refundUpdated) find their row by metadata.refund_id and
// apply only when the event's refund id equals the stored stripe_refund_id,
// or no id is stored yet and metadata.attempt equals the row's attempt. That
// keeps a late "failed" for an attempt the row has moved past from failing
// the retry that replaced it.
func (s *Service) finishRefund(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, stripeStatus, failureReason string) error {
	var order uuid.UUID
	if err := tx.QueryRow(ctx, `select p.order_id from refunds r join payments p on p.id = r.payment_id where r.id = $1`, id).Scan(&order); err != nil {
		return err
	}
	var email, event, organizer string
	var cancelled bool
	if err := tx.QueryRow(ctx, `select o.email, e.name, g.name, e.status = 'cancelled' from orders o join events e on e.id = o.event_id
		join organizers g on g.id = o.organizer_id where o.id = $1 for update of o`, order).Scan(&email, &event, &organizer, &cancelled); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select 1 from tickets where order_id = $1 and refund_id = $2 order by id for update`, order, id); err != nil {
		return err
	}
	var status, reason string
	var failure *string
	var amount, fee int64
	var feeDone bool
	// fee is 0 when the payment took none (direct mode): nothing to give back.
	if err := tx.QueryRow(ctx, `select status, failure, reason, amount_cents,
			case when (select application_fee_cents from payments where id = refunds.payment_id) > 0 then fee_cents else 0 end,
			fee_refunded_at is not null from refunds where id = $1 for update`, id).
		Scan(&status, &failure, &reason, &amount, &fee, &feeDone); err != nil {
		return err
	}
	switch stripeStatus {
	case "succeeded":
		// We may have given up asking while the money went out after all.
		gaveUp := status == "failed" && failure != nil && *failure == "gave_up"
		if status != "requested" && !gaveUp {
			return nil
		}
		if _, err := tx.Exec(ctx, `update refunds set status = 'succeeded', failure = null, updated_at = statement_timestamp() where id = $1`, id); err != nil {
			return err
		}
		if gaveUp {
			if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "refund.succeeded_late",
				EntityType: "refund", EntityID: id, Before: map[string]string{"failure": "gave_up"}}); err != nil {
				return err
			}
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
		subject, text := refundEmail(reason, cancelled, event, organizer, amount)
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": email, "subject": subject, "text": text})
	case "failed", "canceled":
		// A row we gave up on, or couldn't reach, becomes stripe_failed:
		// Stripe's word is final, so a retry moves to the next key.
		if status == "failed" && *failure == "stripe_failed" {
			return nil
		}
		if status == "succeeded" {
			// The money is back in the organizer's balance. Seats stay
			// released and our fee stays returned; the organizer sees it
			// failed and can retry.
			slog.ErrorContext(ctx, "refund: Stripe failed a refund it had called succeeded", "organizer", org, "order", order, "refund", id)
		}
		return s.failRefund(ctx, tx, org, id, "stripe_failed", failureReason)
	}
	return nil
}

// failRefund marks a refund failed and says so in the organizer's audit
// log, with what it was before and Stripe's failure_reason when there is
// one. A failed row changes only to stripe_failed, from gave_up or
// no_access, and a succeeded one only to stripe_failed. The caller holds
// the order lock or needs none (the row alone).
func (s *Service) failRefund(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, failure, stripeReason string) error {
	var status string
	var was *string
	if err := tx.QueryRow(ctx, `select status, failure from refunds where id = $1 for update`, id).Scan(&status, &was); err != nil {
		return err
	}
	if status == "failed" && (failure != "stripe_failed" || *was == "stripe_failed") {
		return nil
	}
	// The money went out: only Stripe's own word can say it didn't. A read
	// made before the row was locked (RefundParked, a lost account) mustn't.
	if status == "succeeded" && failure != "stripe_failed" {
		return nil
	}
	// A succeeded refund fails only on Stripe's word: the buyer's bank sent it back.
	if _, err := tx.Exec(ctx, `update refunds set status = 'failed', failure = $2, returned_by_bank = $3, updated_at = statement_timestamp()
		where id = $1`, id, failure, status == "succeeded"); err != nil {
		return err
	}
	before := map[string]string{"status": status}
	if was != nil {
		before["failure"] = *was
	}
	after := map[string]string{"failure": failure}
	if stripeReason != "" {
		after["failure_reason"] = stripeReason
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "refund.failed",
		EntityType: "refund", EntityID: id, Before: before, After: after})
}

// RefundParked is the queue's word that a refund job ran out of tries.
// Nothing here names the buyer.
//   - Not yet with Stripe (no Stripe id): the buyer may still be owed money,
//     so the refund is failed (gave_up) for the organizer to see and retry,
//     with an ERROR naming the organizer, order and refund. Stripe's later
//     word still finishes it if the money went out after all.
//   - With Stripe, waiting for its word: left requested for the webhook.
//   - Succeeded with our fee still owed: a refund.fee_stuck audit row.
//
// The queue parks only real failures, never a shutdown (that hands the job
// back), and failRefund is a no-op on a failed row, so this raises once.
func RefundParked(s *Service) func(ctx context.Context, payload []byte, cause string) {
	return func(ctx context.Context, payload []byte, cause string) {
		ctx = context.WithoutCancel(ctx)
		var j refundJob
		if err := json.Unmarshal(payload, &j); err != nil {
			slog.ErrorContext(ctx, "refund stuck: unreadable job", "err", err)
			return
		}
		var r refundRow
		var marked string
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) (err error) {
			if r, err = loadRefund(ctx, tx, j.Refund); err != nil {
				return err
			}
			switch {
			case r.status == "requested" && r.stripeID == nil:
				marked = "gave up"
				return s.failRefund(ctx, tx, j.Organizer, j.Refund, "gave_up", "")
			case r.status == "succeeded" && r.fee > 0 && r.feeTaken && !r.feeDone:
				marked = "fee stuck"
				return feeStuck(ctx, tx, j.Organizer, r)
			}
			return nil
		})
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "refund stuck: not marked", "organizer", j.Organizer, "refund", j.Refund, "err", err, "cause", cause)
		case marked != "":
			slog.ErrorContext(ctx, "refund stuck: nothing will retry it", "organizer", j.Organizer, "order", r.order, "refund", j.Refund,
				"stuck", marked, "err", cause)
		default:
			slog.WarnContext(ctx, "refund job parked with nothing to mark", "organizer", j.Organizer, "order", r.order, "refund", j.Refund,
				"status", r.status, "err", cause)
		}
	}
}

// refundEmail is what the buyer reads once the money is on its way back. A
// payment that arrived after the event was cancelled is refunded for that.
func refundEmail(reason string, cancelled bool, event, organizer string, amount int64) (subject, text string) {
	subject = "Your refund for " + event
	switch {
	case reason == "late_payment" && cancelled:
		text = event + " was cancelled while your payment was on its way, so we couldn't give you tickets. We've refunded you in full: " +
			dollars(amount) + " back to your card. It usually shows up in 5 to 10 days."
	case reason == "late_payment":
		text = "Sorry, we couldn't keep your tickets for " + event + ". By the time your payment reached us, they weren't available any more, " +
			"so we've refunded you in full: " + dollars(amount) + " back to your card. It usually shows up in 5 to 10 days."
	case reason == "event_cancelled":
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
