package sales

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// MailState is how the order's ticket email is getting on. Status nil: no
// ticket email yet. Problem is the last thing that went wrong, as a word.
type MailState struct {
	Status, Problem *string
	Attempts        int
	UpdatedAt       *time.Time
}

var (
	errEmailOnItsWay = catalogue.ErrConflict{Code: "email_on_its_way",
		Message: "We're still sending the last one. It'll get there; give it a few minutes."}
	errTooSoon = catalogue.ErrConflict{Code: "too_soon",
		Message: "We sent the tickets a few minutes ago. Give that one time to land, and ask the buyer to look in spam before you send again."}
	errNoTickets = catalogue.ErrConflict{Code: "no_tickets",
		Message: "This order has no valid tickets left, so there's nothing to send."}
)

// queueTicketEmail queues the order's ticket email through
// queue_order_email (00035), which holds every rule; this only names its answer.
func queueTicketEmail(ctx context.Context, tx pgx.Tx, order uuid.UUID) error {
	var res *string
	if err := tx.QueryRow(ctx, `select queue_order_email($1)`, order).Scan(&res); err != nil {
		return err
	}
	switch {
	case res == nil:
		return catalogue.ErrNotFound
	case *res == "on_its_way":
		return errEmailOnItsWay
	case *res == "too_soon":
		return errTooSoon
	case *res == "no_tickets":
		return errNoTickets
	}
	return nil
}

// ResendTicketEmail sends an order's ticket email again: the same job, to
// the same address, with the same link. Runs in the caller's organizer
// transaction.
func ResendTicketEmail(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, actor audit.Actor) (MailState, error) {
	var before *string
	err := tx.QueryRow(ctx, `select mail_status from orders where id = $1 and event_id = $2`, order, event).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return MailState{}, catalogue.ErrNotFound
	}
	if err != nil {
		return MailState{}, err
	}
	if err := queueTicketEmail(ctx, tx, order); err != nil {
		return MailState{}, err
	}
	if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "order.email_resent", EntityType: "order", EntityID: order,
		Before: map[string]*string{"mail_status": before}}); err != nil {
		return MailState{}, err
	}
	var st MailState
	err = tx.QueryRow(ctx, `select mail_status, mail_problem, mail_attempts, mail_updated_at from orders where id = $1`, order).
		Scan(&st.Status, &st.Problem, &st.Attempts, &st.UpdatedAt)
	return st, err
}

// recordSend writes how a ticket email's send went, over a status of queued
// or failing only: a webhook's delivered (or bounced) may have landed first.
func (s *Service) recordSend(ctx context.Context, c confirmation, sendErr error) error {
	status, problem := "sent", (*string)(nil)
	if sendErr != nil {
		p := mailer.ProblemOf(sendErr)
		problem = &p
		me, _ := errors.AsType[*mailer.Error](sendErr)
		switch {
		case me != nil && me.Kind == mailer.DailyQuota:
			status = "failing" // delayed, not parked, whatever the attempt: it can still go
		case me != nil && me.Kind == mailer.Permanent, jobs.Attempt(ctx) >= jobs.MaxAttempts:
			status = "gave_up"
		default:
			status = "failing"
		}
	}
	return system.Run(ctx, s.Pool, c.Organizer, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update orders set mail_status = $2, mail_problem = $3, mail_attempts = $4, mail_updated_at = statement_timestamp()
			where id = $1 and mail_status in ('queued', 'failing')`, c.Order, status, problem, jobs.Attempt(ctx))
		return err
	})
}

// nothingToSend is the job finding no ticket email to send (the order isn't
// paid any more, or has no valid ticket): the status goes back to none, so
// the order isn't left on its way forever.
func nothingToSend(ctx context.Context, tx pgx.Tx, order uuid.UUID) error {
	_, err := tx.Exec(ctx, `update orders set mail_status = null, mail_problem = null, mail_updated_at = statement_timestamp()
		where id = $1 and mail_status in ('queued', 'failing')`, order)
	return err
}

// TicketEmailParked is the order_confirmation job's Parked hook: the queue
// gave up on it (a crash every time), so the order says so.
func TicketEmailParked(s *Service) func(ctx context.Context, payload []byte, cause string) {
	return func(ctx context.Context, payload []byte, _ string) {
		var c confirmation
		if err := json.Unmarshal(payload, &c); err != nil {
			slog.ErrorContext(ctx, "ticket email parked: bad payload", "err", err)
			return
		}
		if err := system.Run(ctx, s.Pool, c.Organizer, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update orders set mail_status = 'gave_up', mail_attempts = $2, mail_updated_at = statement_timestamp()
				where id = $1 and mail_status in ('queued', 'failing')`, c.Order, jobs.MaxAttempts)
			return err
		}); err != nil {
			slog.ErrorContext(ctx, "ticket email parked: status not saved", "order", c.Order, "err", err)
		}
	}
}
