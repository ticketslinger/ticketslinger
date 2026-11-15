package sales

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

var (
	errPriceDiffers error = catalogue.ErrConflict{Code: "price_differs",
		Message: "Those tickets cost different amounts, so we can't swap one for the other. Refund this ticket, then add the new one as a manual order."}
	errSameType       = invalid("same_type", "ticket_type_id", "It's already that type.")
	errChangeTypeType = invalid("unknown_ticket_type", "ticket_type_id", "That ticket type isn't part of this event. Reload the page and pick again.")
)

// ChangeTicketType moves a valid ticket of order of event, not checked in,
// to type to of the same event: same price and tax, unless the order is a
// comp. Its QR, name and answers stay. It takes a seat in each of to's
// quotas the old type isn't in, and then asks queue_order_email to send the
// tickets again; email is what that said, and the change stands whatever it
// was. Runs in the caller's transaction; locks the order, the ticket, then
// the quotas of both types.
func ChangeTicketType(ctx context.Context, tx pgx.Tx, org, event, order, ticket, to uuid.UUID, actor audit.Actor) (email string, err error) {
	var source string
	err = tx.QueryRow(ctx, `select source from orders where id = $1 and event_id = $2 for update`, order, event).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", catalogue.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var from uuid.UUID
	var valid, in bool
	err = tx.QueryRow(ctx, `select ticket_type_id, status = 'valid', checked_in_at is not null from tickets
		where id = $1 and order_id = $2 for update`, ticket, order).Scan(&from, &valid, &in)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", catalogue.ErrNotFound
	case err != nil:
		return "", err
	case !valid:
		return "", errTicketVoidNow
	case in:
		return "", ErrCheckedIn
	case to == from:
		return "", errSameType
	}
	// Price and tax before any quota work: a dearer type is price_differs,
	// full or not. Tax counts too, so the ticket's stored tax stays true.
	var name string
	var differs bool
	err = tx.QueryRow(ctx, `select n.name, n.price_cents <> c.price_cents
			or coalesce(n.tax_rate_bp, e.tax_rate_bp) <> coalesce(c.tax_rate_bp, e.tax_rate_bp)
			or coalesce(n.prices_include_tax, e.prices_include_tax) <> coalesce(c.prices_include_tax, e.prices_include_tax)
		from ticket_types n join ticket_types c on c.id = $2 join events e on e.id = n.event_id
		where n.id = $1 and n.event_id = $3`, to, from, event).Scan(&name, &differs)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", errChangeTypeType
	case err != nil:
		return "", err
	case differs && source != "comp":
		return "", errPriceDiffers
	}
	sizes, links, err := lockQuotas(ctx, tx, event, []uuid.UUID{from, to}) // in quota id order
	if errors.Is(err, errQuotasMoved) {
		return "", ErrBusy
	}
	if err != nil {
		return "", err
	}
	if len(links[to]) == 0 {
		return "", errNoQuotaType(name)
	}
	// Not fits: its refusal reads event_availability, which ts_app can't.
	for _, q := range links[to] {
		if slices.Contains(links[from], q) {
			continue // the seat stays in it
		}
		var used int
		if err := tx.QueryRow(ctx, `select quota_used($1, null)`, q).Scan(&used); err != nil {
			return "", err
		}
		if used+1 > sizes[q] {
			return "", soldOutAll(ctx, tx, event)
		}
	}
	if _, err := tx.Exec(ctx, `select set_ticket_type($1, $2)`, ticket, to); err != nil {
		return "", typeError(err)
	}
	if err := tx.QueryRow(ctx, `select queue_order_email($1)`, order).Scan(&email); err != nil {
		return "", err
	}
	return email, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket.type_changed", EntityType: "ticket", EntityID: ticket,
		Before: map[string]uuid.UUID{"ticket_type_id": from}, After: map[string]uuid.UUID{"ticket_type_id": to}})
}

// typeError maps set_ticket_type's refusals, which the checks above should
// already have made; a price edited since is the one that can get through.
func typeError(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "22023" {
		return err
	}
	switch pg.Message {
	case "same_type":
		return errSameType
	case "checked_in":
		return ErrCheckedIn
	case "ticket_void":
		return errTicketVoidNow
	case "price_differs":
		return errPriceDiffers
	case "unknown_ticket_type":
		return errChangeTypeType
	}
	return err
}
