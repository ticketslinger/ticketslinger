package catalogue

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

type TicketType struct {
	ID               uuid.UUID  `json:"id"`
	EventID          uuid.UUID  `json:"event_id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	PriceCents       int64      `json:"price_cents"`
	TaxRateBP        *int       `json:"tax_rate_bp"` // nil = the event's rate
	PricesIncludeTax *bool      `json:"prices_include_tax"`
	SalesStart       *time.Time `json:"sales_start"`
	SalesEnd         *time.Time `json:"sales_end"`
	MinPerOrder      int        `json:"min_per_order"`
	MaxPerOrder      int        `json:"max_per_order"`
	Hidden           bool       `json:"hidden"`
	Position         int        `json:"position"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// TicketTypeInput is a new ticket type. A nil MinPerOrder or MaxPerOrder
// means 1 and 10; any value that is sent has to be in range.
type TicketTypeInput struct {
	Name, Description string
	PriceCents        int64
	TaxRateBP         *int
	PricesIncludeTax  *bool
	SalesStart        *time.Time
	SalesEnd          *time.Time
	MinPerOrder       *int
	MaxPerOrder       *int
	Hidden            bool
}

// TicketTypePatch changes only the fields that are set. For the four
// nullable fields, absent leaves the value alone and an explicit null clears it.
type TicketTypePatch struct {
	Name, Description *string
	PriceCents        *int64
	TaxRateBP         nullable.Nullable[int]
	PricesIncludeTax  nullable.Nullable[bool]
	SalesStart        nullable.Nullable[time.Time]
	SalesEnd          nullable.Nullable[time.Time]
	MinPerOrder       *int
	MaxPerOrder       *int
	Hidden            *bool
}

const ttCols = `id, event_id, name, description, price_cents, tax_rate_bp, prices_include_tax, sales_start, sales_end,
	min_per_order, max_per_order, hidden, position, created_at, updated_at`

func scanTT(row pgx.Row) (TicketType, error) {
	var t TicketType
	err := row.Scan(&t.ID, &t.EventID, &t.Name, &t.Description, &t.PriceCents, &t.TaxRateBP, &t.PricesIncludeTax, &t.SalesStart, &t.SalesEnd,
		&t.MinPerOrder, &t.MaxPerOrder, &t.Hidden, &t.Position, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketType{}, ErrNotFound
	}
	return t, err
}

// validateTT holds the rules the CHECK constraints in 00008 back up.
func validateTT(t *TicketType) error {
	switch {
	case utf8.RuneCountInString(t.Name) < 1 || utf8.RuneCountInString(t.Name) > 120:
		return invalid("invalid_name", "Give the ticket type a name of up to 120 characters.")
	case utf8.RuneCountInString(t.Description) > 2000:
		return invalid("invalid_description", "The description can run to 2,000 characters. This one is longer.")
	case t.PriceCents < 0 || t.PriceCents > 10000000:
		return invalid("bad_price", "A price runs from $0 to $100,000. Send it in cents: $25 is 2500.")
	case t.TaxRateBP != nil && (*t.TaxRateBP < 0 || *t.TaxRateBP > 10000):
		return invalid("invalid_tax_rate", "Tax is in basis points, from 0 to 10000. For 8.25%, send 825.")
	case t.MinPerOrder < 1 || t.MaxPerOrder < 1 || t.MinPerOrder > 100 || t.MaxPerOrder > 100 || t.MinPerOrder > t.MaxPerOrder:
		return invalid("bad_limits", "Per-order limits run from 1 to 100, and the minimum can't be above the maximum.")
	case t.SalesStart != nil && t.SalesEnd != nil && !t.SalesEnd.After(*t.SalesStart):
		return invalid("bad_sales_window", "Sales have to end after they start.")
	}
	return nil
}

// ttDBError is dbError for ticket types: 23514 is a failed CHECK.
func ttDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{"invalid_ticket_type", "Something in that ticket type isn't allowed. Check the lengths, price, limits and dates."}
		case "23505":
			return ErrConflict{"ticket_type_exists", "That ticket type already exists."}
		}
	}
	return err
}

// lockEvent locks the scoped organizer's event row, so creates, deletes and
// going live line up one behind the other. Another tenant's event is
// ErrNotFound.
func lockEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Event, error) {
	return scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1 for update`, id))
}

// CreateTicketType adds a ticket type at the end of the event's list and
// records ticket_type.created by actor. tx must be scoped to org.
func CreateTicketType(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, in TicketTypeInput) (TicketType, error) {
	t := TicketType{Name: in.Name, Description: in.Description, PriceCents: in.PriceCents, TaxRateBP: in.TaxRateBP, PricesIncludeTax: in.PricesIncludeTax,
		SalesStart: in.SalesStart, SalesEnd: in.SalesEnd, MinPerOrder: 1, MaxPerOrder: 10, Hidden: in.Hidden}
	if in.MinPerOrder != nil {
		t.MinPerOrder = *in.MinPerOrder
	}
	if in.MaxPerOrder != nil {
		t.MaxPerOrder = *in.MaxPerOrder
	}
	if err := validateTT(&t); err != nil {
		return TicketType{}, err
	}
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return TicketType{}, err
	}
	t, err := scanTT(tx.QueryRow(ctx, `insert into ticket_types(organizer_id, event_id, name, description, price_cents, tax_rate_bp, prices_include_tax,
			sales_start, sales_end, min_per_order, max_per_order, hidden, position)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			(select coalesce(max(position), 0) + 1 from ticket_types where event_id = $2)) returning `+ttCols,
		org, event, t.Name, t.Description, t.PriceCents, t.TaxRateBP, t.PricesIncludeTax, t.SalesStart, t.SalesEnd, t.MinPerOrder, t.MaxPerOrder, t.Hidden))
	if err != nil {
		return TicketType{}, ttDBError(err)
	}
	return t, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.created", EntityType: "ticket_type", EntityID: t.ID, After: t})
}

func GetTicketType(ctx context.Context, tx pgx.Tx, event, id uuid.UUID) (TicketType, error) {
	return scanTT(tx.QueryRow(ctx, `select `+ttCols+` from ticket_types where id = $1 and event_id = $2`, id, event))
}

// ListTicketTypes returns the event's ticket types in position order.
func ListTicketTypes(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]TicketType, error) {
	if _, err := GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select `+ttCols+` from ticket_types where event_id = $1 order by position, created_at, id`, event)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TicketType, error) { return scanTT(r) })
}

// UpdateTicketType applies patch to the locked row, validates the merged
// result and records ticket_type.updated. A patch that changes nothing writes
// nothing.
func UpdateTicketType(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor, p TicketTypePatch) (TicketType, error) {
	before, err := scanTT(tx.QueryRow(ctx, `select `+ttCols+` from ticket_types where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return TicketType{}, err
	}
	t := before
	if p.Name != nil {
		t.Name = *p.Name
	}
	if p.Description != nil {
		t.Description = *p.Description
	}
	if p.PriceCents != nil {
		t.PriceCents = *p.PriceCents
	}
	setNull(&t.TaxRateBP, p.TaxRateBP)
	setNull(&t.PricesIncludeTax, p.PricesIncludeTax)
	setNull(&t.SalesStart, p.SalesStart)
	setNull(&t.SalesEnd, p.SalesEnd)
	if p.MinPerOrder != nil {
		t.MinPerOrder = *p.MinPerOrder
	}
	if p.MaxPerOrder != nil {
		t.MaxPerOrder = *p.MaxPerOrder
	}
	if p.Hidden != nil {
		t.Hidden = *p.Hidden
	}
	if err := validateTT(&t); err != nil {
		return TicketType{}, err
	}
	if sameTT(t, before) {
		return before, nil
	}
	// ponytail: P3 must guard price and limit edits once tickets are sold.
	t, err = scanTT(tx.QueryRow(ctx, `update ticket_types set name = $2, description = $3, price_cents = $4, tax_rate_bp = $5, prices_include_tax = $6,
			sales_start = $7, sales_end = $8, min_per_order = $9, max_per_order = $10, hidden = $11, updated_at = clock_timestamp()
		where id = $1 returning `+ttCols,
		id, t.Name, t.Description, t.PriceCents, t.TaxRateBP, t.PricesIncludeTax, t.SalesStart, t.SalesEnd, t.MinPerOrder, t.MaxPerOrder, t.Hidden))
	if err != nil {
		return TicketType{}, ttDBError(err)
	}
	return t, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.updated", EntityType: "ticket_type", EntityID: id, Before: before, After: t})
}

// setNull applies one nullable patch field: null clears, a value sets,
// absent does nothing.
func setNull[T any](dst **T, n nullable.Nullable[T]) {
	if n.IsNull() {
		*dst = nil
	} else if v, err := n.Get(); err == nil {
		*dst = &v
	}
}

// sameTT: a patch that changed nothing. Instants compare by Equal.
func sameTT(a, b TicketType) bool {
	eq := func(x, y *time.Time) bool { return (x == nil && y == nil) || (x != nil && y != nil && x.Equal(*y)) }
	if !eq(a.SalesStart, b.SalesStart) || !eq(a.SalesEnd, b.SalesEnd) {
		return false
	}
	a.SalesStart, a.SalesEnd, b.SalesStart, b.SalesEnd = nil, nil, nil, nil
	same := func(x, y *int) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	if !same(a.TaxRateBP, b.TaxRateBP) {
		return false
	}
	a.TaxRateBP, b.TaxRateBP = nil, nil
	if (a.PricesIncludeTax == nil) != (b.PricesIncludeTax == nil) || (a.PricesIncludeTax != nil && *a.PricesIncludeTax != *b.PricesIncludeTax) {
		return false
	}
	a.PricesIncludeTax, b.PricesIncludeTax = nil, nil
	return a == b
}

// DeleteTicketType removes a ticket type and records ticket_type.deleted.
// The last ticket type of a live event stays: ErrConflict last_ticket_type_live.
func DeleteTicketType(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor) error {
	ev, err := lockEvent(ctx, tx, event)
	if err != nil {
		return err
	}
	before, err := scanTT(tx.QueryRow(ctx, `select `+ttCols+` from ticket_types where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return err
	}
	if ev.Status == "live" {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from ticket_types where event_id = $1`, event).Scan(&n); err != nil {
			return err
		}
		if n == 1 {
			return ErrConflict{"last_ticket_type_live", "Move the event back to draft before removing its last ticket type."}
		}
	}
	// ponytail: P3 must guard this once tickets are sold.
	if _, err := tx.Exec(ctx, `delete from ticket_types where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.deleted", EntityType: "ticket_type", EntityID: id, Before: before})
}
