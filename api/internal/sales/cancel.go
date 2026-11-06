package sales

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
)

// CancelTicketsRequest cancels tickets of an order without a refund.
type CancelTicketsRequest struct {
	TicketIDs []uuid.UUID // nil: every valid ticket; empty, non-nil: bad_tickets
	Notify    bool
	Reason    string // shown to the buyer, up to 500 characters
}

type CancelTicketsResult struct {
	TicketIDs   []uuid.UUID
	ChargeCents int64 // what the buyer paid for them; none of it moves
	OrderStatus string
}

var (
	errNotCancellable = catalogue.ErrConflict{Code: "not_cancellable", Message: "This order has no valid tickets left to cancel."}
	errCancelTickets  = catalogue.ErrInvalid{Code: "bad_tickets", Field: "ticket_ids",
		Message: "Some of those tickets aren't in this order, or they're already refunded or cancelled. Reload the order and pick again."}
	errBadReason = invalid("bad_reason", "reason", "Keep the reason to 500 characters of plain text.")
)

// CancelTickets voids tickets of one order of event and frees their seats,
// with no refund: the payment stands. Checked-in tickets can go too. Runs in
// the caller's transaction; locks the order, then its tickets, as
// RefundTickets does.
func CancelTickets(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, in CancelTicketsRequest, actor audit.Actor) (CancelTicketsResult, error) {
	return cancelTickets(ctx, tx, org, event, order, in, "organizer", actor)
}

func cancelTickets(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, in CancelTicketsRequest, via string, actor audit.Actor) (CancelTicketsResult, error) {
	switch {
	case len(in.TicketIDs) > MaxRefundTickets:
		return CancelTicketsResult{}, errTooManyTickets
	case in.TicketIDs != nil && len(in.TicketIDs) == 0:
		return CancelTicketsResult{}, errCancelTickets
	}
	reason, ok := cleanText(in.Reason, 500)
	if !ok {
		return CancelTicketsResult{}, errBadReason
	}
	var status, source, method string
	var email *string
	err := tx.QueryRow(ctx, `select status, source, coalesce(manual_method, ''), email::text from orders where id = $1 and event_id = $2 for update`, order, event).
		Scan(&status, &source, &method, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelTicketsResult{}, catalogue.ErrNotFound
	}
	if err != nil {
		return CancelTicketsResult{}, err
	}
	if status != "paid" && status != "partially_refunded" {
		return CancelTicketsResult{}, errNotCancellable
	}
	var named []uuid.UUID
	if len(in.TicketIDs) > 0 {
		named = slices.Compact(slices.SortedFunc(slices.Values(in.TicketIDs), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }))
	}
	rows, _ := tx.Query(ctx, `select t.id, tt.name, coalesce(t.attendee_name, ''), t.charge_cents from tickets t
		join ticket_types tt on tt.id = t.ticket_type_id
		where t.order_id = $1 and t.status = 'valid' and ($2::uuid[] is null or t.id = any($2)) order by t.id for update of t`, order, named)
	res := CancelTicketsResult{}
	var lines []string
	var id uuid.UUID
	var typ, attendee string
	var charge int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &typ, &attendee, &charge}, func() error {
		res.TicketIDs = append(res.TicketIDs, id)
		res.ChargeCents += charge
		line := "- " + typ
		if attendee != "" {
			line += ", " + attendee
		}
		lines = append(lines, line)
		return nil
	}); err != nil {
		return CancelTicketsResult{}, err
	}
	switch {
	case named != nil && len(res.TicketIDs) != len(named):
		return CancelTicketsResult{}, errCancelTickets
	case len(res.TicketIDs) == 0:
		return CancelTicketsResult{}, errNotCancellable
	}
	if _, err := tx.Exec(ctx, `update tickets set status = 'void', released_at = statement_timestamp() where id = any($1)`, res.TicketIDs); err != nil {
		return CancelTicketsResult{}, err
	}
	if res.OrderStatus, err = rollUpOrder(ctx, tx, order); err != nil {
		return CancelTicketsResult{}, err
	}
	if in.Notify && email != nil && *email != "" {
		var eventName, organizer string
		if err := tx.QueryRow(ctx, `select e.name, g.name from events e join organizers g on g.id = e.organizer_id where e.id = $1`, event).Scan(&eventName, &organizer); err != nil {
			return CancelTicketsResult{}, err
		}
		subject, text := cancelEmail(eventName, organizer, reason, lines, source, method, res.ChargeCents)
		if err := jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": *email, "subject": subject, "text": text}); err != nil {
			return CancelTicketsResult{}, err
		}
	}
	return res, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "order.cancelled", EntityType: "order", EntityID: order,
		After: map[string]any{"ticket_ids": res.TicketIDs, "notify": in.Notify, "reason": reason, "via": via}})
}

// cancelEmail tells a buyer which tickets were cancelled. A comp or an
// invoice wasn't paid, so neither says nothing was refunded; an invoice says
// the organizer will be in touch about the cents those tickets came to.
func cancelEmail(event, organizer, reason string, lines []string, source, method string, cents int64) (subject, text string) {
	subject = "Tickets cancelled for " + event
	text = organizer + " has cancelled these tickets for " + event + ", so they won't get anyone in any more:\n\n" + strings.Join(lines, "\n") + "\n\n"
	if reason != "" {
		text += "Their reason: " + reason + "\n\n"
	}
	switch {
	case source == "manual" && method == "invoice":
		if cents > 0 {
			text += invoiceLine(organizer, cents) + " "
		}
	case source != "comp":
		text += "Nothing has been refunded. "
	}
	return subject, text + "Questions? Contact the organizer, " + organizer + "."
}

// invoiceLine is what a buyer of an invoiced order is told about its money.
func invoiceLine(organizer string, cents int64) string {
	return organizer + " will be in touch about the " + dollars(cents) + " invoice."
}
