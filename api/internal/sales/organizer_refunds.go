package sales

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// RefundRequest is an organizer's refund of an order.
type RefundRequest struct {
	TicketIDs []uuid.UUID // nil: every ticket still valid; empty but not nil names none (bad_tickets)
	FeeBack   bool        // our fee on those tickets goes back too
}

type RefundResult struct {
	RefundID              *uuid.UUID // nil when those tickets cost nothing
	TicketIDs             []uuid.UUID
	AmountCents, FeeCents int64
	OrderStatus           string
}

// MaxRefundTickets is how many tickets one refund may name.
const MaxRefundTickets = 100

var (
	errNotRefundable = catalogue.ErrConflict{Code: "not_refundable", Message: "This order has nothing left to refund."}
	errBadTickets    = catalogue.ErrInvalid{Code: "bad_tickets", Field: "ticket_ids",
		Message: "Some of those tickets aren't in this order, or they're refunded already. Reload the order and pick again."}
	errTooManyTickets = catalogue.ErrInvalid{Code: "too_many", Field: "ticket_ids",
		Message: "Pick up to 100 tickets at a time, or leave the list out to refund the whole order."}
	errNotFailed = catalogue.ErrConflict{Code: "not_failed", Message: "Only a refund that failed can be sent again."}
	errNoRoom    = catalogue.ErrConflict{Code: "no_room",
		Message: "Other refunds on this order have taken what this one needed since it failed, so there isn't enough left to send it again."}
	errFeeNoMoney = catalogue.ErrInvalid{Code: "fee_without_refund", Field: "refund_fee",
		Message: "Those tickets cost nothing, so there's no payment to send our fee back through. Refund them with refund_fee false."}
)

// RefundTickets voids tickets of one order of event and asks Stripe for the
// money back. Runs in the caller's transaction; locks the order, then its
// tickets. reason: "organizer", "event_cancelled" or "platform".
func RefundTickets(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, in RefundRequest, reason string, actor audit.Actor) (RefundResult, error) {
	switch {
	case len(in.TicketIDs) > MaxRefundTickets:
		return RefundResult{}, errTooManyTickets
	case in.TicketIDs != nil && len(in.TicketIDs) == 0:
		return RefundResult{}, errBadTickets
	}
	var status, feeMode string
	err := tx.QueryRow(ctx, `select status, fee_mode from orders where id = $1 and event_id = $2 for update`, order, event).Scan(&status, &feeMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{}, catalogue.ErrNotFound
	}
	if err != nil {
		return RefundResult{}, err
	}
	if status != "paid" && status != "partially_refunded" {
		return RefundResult{}, errNotRefundable
	}
	var named []uuid.UUID
	if len(in.TicketIDs) > 0 {
		named = slices.Compact(slices.SortedFunc(slices.Values(in.TicketIDs), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }))
	}
	rows, _ := tx.Query(ctx, `select id, charge_cents, fee_cents from tickets
		where order_id = $1 and status = 'valid' and ($2::uuid[] is null or id = any($2)) order by id for update`, order, named)
	res := RefundResult{}
	var charges, fees int64
	var id uuid.UUID
	var charge, fee int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &charge, &fee}, func() error {
		res.TicketIDs = append(res.TicketIDs, id)
		charges, fees = charges+charge, fees+fee
		return nil
	}); err != nil {
		return RefundResult{}, err
	}
	switch {
	case named != nil && len(res.TicketIDs) != len(named):
		return RefundResult{}, errBadTickets
	case len(res.TicketIDs) == 0:
		return RefundResult{}, errNotRefundable
	}
	res.AmountCents = charges
	if in.FeeBack {
		res.FeeCents = fees
		if feeMode == pricing.FeePass {
			res.AmountCents += fees
		}
	}
	if res.AmountCents == 0 && res.FeeCents > 0 {
		return RefundResult{}, errFeeNoMoney
	}
	if res.AmountCents > 0 {
		var payment uuid.UUID
		if err := tx.QueryRow(ctx, `select id from payments where order_id = $1 and status = 'succeeded'`, order).Scan(&payment); err != nil {
			return RefundResult{}, err
		}
		var by *uuid.UUID
		if actor.User != uuid.Nil {
			by = &actor.User
		}
		refund, err := requestRefund(ctx, tx, org, payment, res.AmountCents, res.FeeCents, in.FeeBack, reason, by)
		if err != nil {
			return RefundResult{}, err
		}
		res.RefundID = &refund
		if _, err := tx.Exec(ctx, `update tickets set status = 'void', refund_id = $2 where id = any($1)`, res.TicketIDs, refund); err != nil {
			return RefundResult{}, err
		}
	} else if _, err := tx.Exec(ctx, `update tickets set status = 'void', released_at = statement_timestamp() where id = any($1)`, res.TicketIDs); err != nil {
		return RefundResult{}, err
	}
	if err := tx.QueryRow(ctx, `update orders set status = case
			when exists (select 1 from tickets where order_id = $1 and status = 'valid') then 'partially_refunded'
			when total_cents > 0 then 'refunded' else 'cancelled' end
		where id = $1 returning status`, order).Scan(&res.OrderStatus); err != nil {
		return RefundResult{}, err
	}
	return res, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "order.refunded", EntityType: "order", EntityID: order,
		After: map[string]any{"ticket_ids": res.TicketIDs, "amount_cents": res.AmountCents, "fee_cents": res.FeeCents, "reason": reason, "refund_id": res.RefundID}})
}

// RetryRefund sends a failed refund again: the same row, back to requested,
// with a new job. It keeps its key family (refund-<id>-<attempt>), and the
// job looks at Stripe before sending a row it sent before, so a first try
// Stripe made after all is taken up, not made twice. Only when Stripe itself
// failed the refund does the row move to its next key, since the old one
// would replay that failure. retry_refund (00030) makes the change: members
// can't write refunds directly. Runs in the caller's transaction; locks the
// order, then the refund.
func RetryRefund(ctx context.Context, tx pgx.Tx, org, event, order, refund uuid.UUID, actor audit.Actor) (RefundView, error) {
	err := tx.QueryRow(ctx, `select 1 from orders where id = $1 and event_id = $2 for update`, order, event).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundView{}, catalogue.ErrNotFound
	}
	if err != nil {
		return RefundView{}, err
	}
	var failure *string
	err = tx.QueryRow(ctx, `select r.failure from refunds r join payments p on p.id = r.payment_id where r.id = $1 and p.order_id = $2`,
		refund, order).Scan(&failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundView{}, catalogue.ErrNotFound
	}
	if err != nil {
		return RefundView{}, err
	}
	var retried bool
	if err := tx.QueryRow(ctx, `select retry_refund($1)`, refund).Scan(&retried); err != nil {
		return RefundView{}, err
	}
	if !retried && failure != nil {
		// It was failed under the order lock, so only the cap (00041) stopped it.
		return RefundView{}, errNoRoom
	}
	if !retried {
		return RefundView{}, errNotFailed
	}
	if err := jobs.Enqueue(ctx, tx, RefundJob, refundJob{Organizer: org, Refund: refund}); err != nil {
		return RefundView{}, err
	}
	if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "refund.retried", EntityType: "refund", EntityID: refund,
		Before: map[string]*string{"failure": failure}}); err != nil {
		return RefundView{}, err
	}
	return RefundByID(ctx, tx, refund)
}

type OrderDetailView struct {
	OrderSummary
	Tickets []OrderDetailTicket
	Refunds []RefundView
}

type OrderDetailTicket struct {
	ID                                   uuid.UUID
	TicketTypeName, AttendeeName, Status string
	ChargeCents, FeeCents                int64
	RefundID                             *uuid.UUID
}

type RefundView struct {
	ID                    uuid.UUID
	AmountCents, FeeCents int64
	FeeBack               bool
	Status, Reason        string
	Failure               *string
	ReturnedByBank        bool // it went through, then the buyer's bank sent it back
	CreatedAt             time.Time
}

const refundColumns = `r.id, r.amount_cents, r.fee_cents, r.fee_refunded, r.status, r.reason, r.failure, r.returned_by_bank, r.created_at`

func scanRefund(r pgx.CollectableRow) (v RefundView, err error) {
	err = r.Scan(&v.ID, &v.AmountCents, &v.FeeCents, &v.FeeBack, &v.Status, &v.Reason, &v.Failure, &v.ReturnedByBank, &v.CreatedAt)
	return v, err
}

// RefundByID is one refund, in the caller's organizer transaction.
func RefundByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (RefundView, error) {
	rows, _ := tx.Query(ctx, `select `+refundColumns+` from refunds r where r.id = $1`, id)
	v, err := pgx.CollectExactlyOneRow(rows, scanRefund)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, catalogue.ErrNotFound
	}
	return v, err
}

// OrderDetail is one order of event with its tickets (in id order) and its
// refunds (newest first), in the caller's organizer transaction.
func OrderDetail(ctx context.Context, tx pgx.Tx, event, order uuid.UUID) (OrderDetailView, error) {
	rows, _ := tx.Query(ctx, orderSummarySelect+` and o.id = $2`, event, order)
	sum, err := pgx.CollectExactlyOneRow(rows, scanOrderSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderDetailView{}, catalogue.ErrNotFound
	}
	if err != nil {
		return OrderDetailView{}, err
	}
	out := OrderDetailView{OrderSummary: sum}
	rows, _ = tx.Query(ctx, `select t.id, tt.name, t.attendee_name, t.status, t.charge_cents, t.fee_cents, t.refund_id
		from tickets t join ticket_types tt on tt.id = t.ticket_type_id where t.order_id = $1 order by t.id`, order)
	if out.Tickets, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (t OrderDetailTicket, err error) {
		err = r.Scan(&t.ID, &t.TicketTypeName, &t.AttendeeName, &t.Status, &t.ChargeCents, &t.FeeCents, &t.RefundID)
		return t, err
	}); err != nil {
		return OrderDetailView{}, err
	}
	rows, _ = tx.Query(ctx, `select `+refundColumns+` from refunds r join payments p on p.id = r.payment_id
		where p.order_id = $1 order by r.created_at desc, r.id desc`, order)
	out.Refunds, err = pgx.CollectRows(rows, scanRefund)
	return out, err
}

type CancelResult struct {
	Orders                int   // orders a refund was made for
	AmountCents, FeeCents int64 // asked back from Stripe in all
	PendingOrders         int   // payments not yet made, stopped
	Failed                []RefundFailure
}

// RefundFailure is an order an event refund left out because the database
// refused it, and why.
type RefundFailure struct {
	Order  uuid.UUID `json:"order_id"`
	Reason string    `json:"reason"`
	err    error
}

var (
	errConfirmMismatch = catalogue.ErrInvalid{Code: "confirm_mismatch", Field: "confirm_name",
		Message: "Type the event's name exactly as it's shown, then cancel."}
	errEventNotLive = catalogue.ErrConflict{Code: "event_not_live",
		Message: "Only an event that's on sale can be cancelled. A draft can simply be deleted."}
)

// CancelEvent takes a live event off sale for good and refunds every paid
// order in full, our fee included. confirmName must be the event's name
// exactly (spaces around it ignored): it's a typed confirmation, checked
// here as well as in the dashboard. Locks the event (FOR NO KEY UPDATE),
// then every open order in id order. A confirmation already holding an
// order finishes first and its tickets are refunded here; one that comes
// after sees the event cancelled and refunds itself as a late payment.
// Each pending order gets a CancelPaymentJob, so an intent nobody has paid
// yet is stopped at Stripe; Pay already refuses a cancelled event.
func CancelEvent(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, confirmName string, actor audit.Actor) (CancelResult, error) {
	var res CancelResult
	if utf8.RuneCountInString(confirmName) > 200 {
		return res, errConfirmMismatch
	}
	var name, status string
	err := tx.QueryRow(ctx, `select name, status from events where id = $1 for no key update`, event).Scan(&name, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, catalogue.ErrNotFound
	}
	if err != nil {
		return res, err
	}
	if strings.TrimSpace(confirmName) != name {
		return res, errConfirmMismatch
	}
	if status != "live" {
		return res, errEventNotLive
	}
	if _, err := tx.Exec(ctx, `update events set status = 'cancelled', updated_at = clock_timestamp() where id = $1`, event); err != nil {
		return res, err
	}
	res, err = refundEvent(ctx, tx, org, event, "event_cancelled", actor)
	if err != nil {
		return res, err
	}
	if len(res.Failed) > 0 { // the organizer cancels again once it's sorted
		return res, res.Failed[0].err
	}
	return res, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.cancelled", EntityType: "event", EntityID: event,
		Before: map[string]string{"status": status}, After: map[string]any{"status": "cancelled", "orders": res.Orders,
			"amount_cents": res.AmountCents, "fee_cents": res.FeeCents, "pending_orders": res.PendingOrders}})
}

// refundEvent refunds every paid order of event in full, our fee included,
// and stops every pending payment with a CancelPaymentJob. Locks every open
// order in id order. Each order is refunded inside a savepoint: one the
// database refuses (say, its payment has nothing left) is rolled back on
// its own and listed in res.Failed, and the rest go ahead.
func refundEvent(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, reason string, actor audit.Actor) (CancelResult, error) {
	res := CancelResult{Failed: []RefundFailure{}}
	// Pending orders are locked too, so a confirmation waits and then sees
	// the event cancelled (or its organizer suspended) and refunds itself;
	// only paid ones are refunded here.
	rows, _ := tx.Query(ctx, `select id, status from orders where event_id = $1
		and status in ('pending', 'paid', 'partially_refunded') order by id for update`, event)
	var paid, pending []uuid.UUID
	var id uuid.UUID
	var st string
	if _, err := pgx.ForEachRow(rows, []any{&id, &st}, func() error {
		if st == "pending" {
			pending = append(pending, id)
		} else {
			paid = append(paid, id)
		}
		return nil
	}); err != nil {
		return res, err
	}
	for _, o := range pending {
		if err := jobs.Enqueue(ctx, tx, CancelPaymentJob, settleJob{Organizer: org, Order: o}); err != nil {
			return res, err
		}
	}
	for _, o := range paid {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return res, err
		}
		r, err := RefundTickets(ctx, sp, org, event, o, RefundRequest{FeeBack: true}, reason, actor)
		if err != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return res, rbErr
			}
			why, ok := refusedOrder(err)
			if !ok {
				return res, err
			}
			if why != "" {
				res.Failed = append(res.Failed, RefundFailure{Order: o, Reason: why, err: err})
			}
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return res, err
		}
		if r.RefundID != nil { // a free order is voided, with nothing to send back
			res.Orders++
		}
		res.AmountCents += r.AmountCents
		res.FeeCents += r.FeeCents
	}
	res.PendingOrders = len(pending)
	return res, nil
}

// refusedOrder says whether err refuses one order only, so the others can
// go ahead, and why ("" for an order with nothing left to refund, which is
// skipped). Only an integrity error (class 23) or a trigger's raise (P0001)
// is one order's; anything else fails the whole refund, to be run again.
func refusedOrder(err error) (why string, ok bool) {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, errPaymentUsedUp):
		return "payment_used_up", true
	case errors.Is(err, errNotRefundable):
		return "", true
	case errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "23") || pg.Code == "P0001"):
		return "database refused it (" + pg.Code + "): " + pg.Message, true
	}
	return "", false
}

// RefundEventForStaff carries out a staff refund_event action on a
// suspended organizer: every paid order of the action's event refunded in
// full, our fee included, and every pending payment stopped. The event's
// status stays as it is. One transaction: claim_staff_refund locks the
// action first, so a second call waits and then fails with already_done;
// a failure anywhere leaves the action unfinished, to be run again. An
// order the database refuses is left out and listed in the result, the
// audit row and the alert, for someone to refund by hand.
//
// The suspension is checked once, at the claim (not_suspended). A lift
// that commits after that doesn't stop a refund already running.
//
// alertTo (TS_MAIL_ALERT_TO) gets the totals once the transaction commits.
func (s *Service) RefundEventForStaff(ctx context.Context, org, action uuid.UUID, alertTo string) (CancelResult, error) {
	var res CancelResult
	err := system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		var event uuid.UUID
		if err := tx.QueryRow(ctx, `select claim_staff_refund($1)`, action).Scan(&event); err != nil {
			return err
		}
		actor := audit.Actor{System: true}
		r, err := refundEvent(ctx, tx, org, event, "platform", actor)
		if err != nil {
			return err
		}
		res = r
		totals := map[string]any{"orders": r.Orders, "amount_cents": r.AmountCents, "fee_cents": r.FeeCents, "pending_orders": r.PendingOrders,
			"failed_orders": r.Failed}
		if _, err := tx.Exec(ctx, `select finish_staff_refund($1, $2)`, action, totals); err != nil {
			return err
		}
		totals["staff_action_id"] = action
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.refunded_by_support", EntityType: "event", EntityID: event, After: totals}); err != nil {
			return err
		}
		var slug, name string
		if err := tx.QueryRow(ctx, `select g.slug::text, e.name from events e join organizers g on g.id = e.organizer_id where e.id = $1`, event).Scan(&slug, &name); err != nil {
			return err
		}
		subject, text := refundFinishedAlert(slug, name, action, r)
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": alertTo, "subject": subject, "text": text})
	})
	return res, err
}

// refundFinishedAlert tells the alert address what a staff refund did.
func refundFinishedAlert(slug, event string, action uuid.UUID, r CancelResult) (subject, text string) {
	subject = "Refund finished on " + slug
	if n := len(r.Failed); n > 0 {
		subject += fmt.Sprintf(", %d order%s to refund by hand", n, map[bool]string{true: "", false: "s"}[n == 1])
	}
	text = fmt.Sprintf("The refund of every order for %s (%s) has gone through. Stripe gets each refund next; watch for failed ones in staff mode.\n\n"+
		"Orders refunded: %d\nBack to buyers: %s\nOur fee returned: %s\nPending payments stopped: %d\nStaff action: %s\n",
		event, slug, r.Orders, dollars(r.AmountCents), dollars(r.FeeCents), r.PendingOrders, action)
	if len(r.Failed) > 0 {
		text += "\nThese orders couldn't be refunded here, so they need refunding by hand:\n"
		for _, f := range r.Failed {
			text += "- " + f.Order.String() + ": " + f.Reason + "\n"
		}
	}
	return subject, text
}
