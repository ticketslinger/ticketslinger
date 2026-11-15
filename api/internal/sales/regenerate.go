package sales

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// RegenerateResult is which tickets got new QR codes, and what
// force_order_email said about sending them: queued or on_its_way.
type RegenerateResult struct {
	TicketIDs []uuid.UUID
	Email     string
}

var (
	errNotRegenerable error = catalogue.ErrConflict{Code: "not_regenerable",
		Message: "Every ticket in this order is checked in, refunded or cancelled, so there's no QR code left to replace."}
	errTooManyRegenerate = catalogue.ErrInvalid{Code: "too_many", Field: "ticket_ids",
		Message: "Pick up to 100 tickets at a time, or leave the list out to replace every QR code in the order."}
)

// RegenerateTickets gives tickets of order of event new secrets, so their
// old QR codes scan as unknown, and emails the buyer the new ones. ticketIDs
// nil means every valid ticket not checked in yet; a named ticket that's void
// or another order's is bad_tickets, and a checked-in one ErrCheckedIn. Runs
// in the caller's transaction; locks the order, then its tickets.
func RegenerateTickets(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, ticketIDs []uuid.UUID, actor audit.Actor) (RegenerateResult, error) {
	switch {
	case len(ticketIDs) > MaxRefundTickets:
		return RegenerateResult{}, errTooManyRegenerate
	case ticketIDs != nil && len(ticketIDs) == 0:
		return RegenerateResult{}, errCancelTickets
	}
	err := tx.QueryRow(ctx, `select 1 from orders where id = $1 and event_id = $2 for update`, order, event).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return RegenerateResult{}, catalogue.ErrNotFound
	}
	if err != nil {
		return RegenerateResult{}, err
	}
	var named []uuid.UUID
	if len(ticketIDs) > 0 {
		named = slices.Compact(slices.SortedFunc(slices.Values(ticketIDs), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }))
	}
	rows, _ := tx.Query(ctx, `select id, status = 'valid', checked_in_at is not null from tickets
		where order_id = $1 and ($2::uuid[] is null or id = any($2)) order by id for update`, order, named)
	var res RegenerateResult
	var found, void, checkedIn int
	var id uuid.UUID
	var valid, in bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &valid, &in}, func() error {
		found++
		switch {
		case !valid:
			void++
		case in:
			checkedIn++
		default:
			res.TicketIDs = append(res.TicketIDs, id)
		}
		return nil
	}); err != nil {
		return RegenerateResult{}, err
	}
	switch {
	case named != nil && (found != len(named) || void > 0):
		return RegenerateResult{}, errCancelTickets
	case named != nil && checkedIn > 0:
		return RegenerateResult{}, ErrCheckedIn
	case len(res.TicketIDs) == 0:
		return RegenerateResult{}, errNotRegenerable
	}
	for _, id := range res.TicketIDs {
		secret := make([]byte, 16)
		_, _ = rand.Read(secret) // never fails
		if _, err := tx.Exec(ctx, `select set_ticket_secret($1, $2)`, id, secret); err != nil {
			return RegenerateResult{}, secretError(err)
		}
	}
	if err := tx.QueryRow(ctx, `select force_order_email($1)`, order).Scan(&res.Email); err != nil {
		return RegenerateResult{}, err
	}
	return res, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket.secret_regenerated", EntityType: "order", EntityID: order,
		After: map[string]any{"ticket_ids": res.TicketIDs}})
}

// secretError maps set_ticket_secret's refusals, which the locks above
// should already have ruled out. Its messages never carry the secret.
func secretError(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "22023" {
		return err
	}
	switch pg.Message {
	case "checked_in":
		return ErrCheckedIn
	case "ticket_void":
		return errTicketVoidNow
	}
	return err // bad_secret: a 16-byte collision, so a 500 and a retry
}
