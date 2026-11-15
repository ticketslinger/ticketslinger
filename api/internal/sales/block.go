package sales

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

var (
	errBlockReason = invalid("bad_reason", "reason", "Keep the reason to 200 characters of plain text.")
	// errBlockVoid is ticket_void, as ErrTicketVoid, in words about the door.
	errBlockVoid error = catalogue.ErrConflict{Code: "ticket_void", Message: "This ticket was refunded or cancelled, so it won't scan either way."}
)

// BlockTicket puts a valid ticket of order on hold, in the caller's
// organizer transaction: the door refuses it and no buyer path shows its QR,
// but it stays sold. reason is for the team. Blocking a blocked ticket is nil
// and changes nothing, reason included.
func BlockTicket(ctx context.Context, tx pgx.Tx, org, event, order, ticket uuid.UUID, reason string, actor audit.Actor) error {
	// Before the lookup, so a bad reason is 422 whatever the ticket.
	reason, ok := cleanText(reason, 200)
	if !ok {
		return errBlockReason
	}
	blocked, _, err := lockForBlock(ctx, tx, event, order, ticket)
	if err != nil || blocked {
		return err
	}
	// blocked_at and blocked_reason together, always: the trigger refuses a
	// new reason without a new blocked_at.
	if _, err := tx.Exec(ctx, `update tickets set blocked_at = statement_timestamp(), blocked_reason = nullif($1, '') where id = $2`, reason, ticket); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket.blocked", EntityType: "ticket", EntityID: ticket,
		After: map[string]string{"reason": reason}})
}

// UnblockTicket takes a ticket off hold; an unblocked one is nil and
// changes nothing.
func UnblockTicket(ctx context.Context, tx pgx.Tx, org, event, order, ticket uuid.UUID, actor audit.Actor) error {
	blocked, reason, err := lockForBlock(ctx, tx, event, order, ticket)
	if err != nil || !blocked {
		return err
	}
	if _, err := tx.Exec(ctx, `update tickets set blocked_at = null, blocked_reason = null where id = $1`, ticket); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket.unblocked", EntityType: "ticket", EntityID: ticket,
		Before: map[string]string{"reason": reason}})
}

// lockForBlock locks a ticket of order of event and says whether it's
// blocked, and why. One that isn't this order's is ErrNotFound; a void one,
// errBlockVoid.
func lockForBlock(ctx context.Context, tx pgx.Tx, event, order, ticket uuid.UUID) (blocked bool, reason string, err error) {
	var status string
	err = tx.QueryRow(ctx, `select status, blocked_at is not null, coalesce(blocked_reason, '') from tickets
		where id = $1 and order_id = $2 and event_id = $3 for update`, ticket, order, event).Scan(&status, &blocked, &reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, "", catalogue.ErrNotFound
	case err != nil:
		return false, "", err
	case status != "valid":
		return false, "", errBlockVoid
	}
	return blocked, reason, nil
}
