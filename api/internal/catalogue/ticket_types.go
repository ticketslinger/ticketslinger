package catalogue

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
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
	// What the buyer's price is worked out with: the event's fee mode and the
	// tax that applies, this type's override or the event's. Not audited.
	FeeMode             string `json:"-"`
	ResolvedTaxBP       int    `json:"-"`
	ResolvedIncludesTax bool   `json:"-"`
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
	// QuotaIDs are the quotas of this event to join. A live event needs at
	// least one; on a draft they are optional.
	QuotaIDs []uuid.UUID
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

// ttCols ends with the event's terms as scalar subqueries, which work in
// RETURNING as well as SELECT.
const ttCols = `id, event_id, name, description, price_cents, tax_rate_bp, prices_include_tax, sales_start, sales_end,
	min_per_order, max_per_order, hidden, position, created_at, updated_at,
	(select e.fee_mode from events e where e.id = ticket_types.event_id),
	coalesce(ticket_types.tax_rate_bp, (select e.tax_rate_bp from events e where e.id = ticket_types.event_id)),
	coalesce(ticket_types.prices_include_tax, (select e.prices_include_tax from events e where e.id = ticket_types.event_id))`

func scanTT(row pgx.Row) (TicketType, error) {
	var t TicketType
	err := row.Scan(&t.ID, &t.EventID, &t.Name, &t.Description, &t.PriceCents, &t.TaxRateBP, &t.PricesIncludeTax, &t.SalesStart, &t.SalesEnd,
		&t.MinPerOrder, &t.MaxPerOrder, &t.Hidden, &t.Position, &t.CreatedAt, &t.UpdatedAt,
		&t.FeeMode, &t.ResolvedTaxBP, &t.ResolvedIncludesTax)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketType{}, ErrNotFound
	}
	return t, err
}

// validateTT holds the rules the CHECK constraints in 00008 and 00021 back up.
// It trims the name first, as validate does for events.
func validateTT(t *TicketType) error {
	t.Name = strings.TrimSpace(t.Name)
	switch {
	case utf8.RuneCountInString(t.Name) < 1 || utf8.RuneCountInString(t.Name) > 120:
		return invalid("invalid_name", "name", "Give the ticket type a name of up to 120 characters.")
	case utf8.RuneCountInString(t.Description) > 2000:
		return invalid("invalid_description", "description", "The description can run to 2,000 characters. This one is longer.")
	case t.PriceCents < 0 || t.PriceCents > 10000000:
		return invalid("bad_price", "price_cents", "A price runs from $0 to $100,000. Send it in cents: $25 is 2500.")
	case t.PriceCents > 0 && t.PriceCents < pricing.MinimumChargeCents:
		return invalid("below_minimum_charge", "price_cents", "A paid ticket has to cost at least $0.50. That's the least a card can be charged.")
	case t.TaxRateBP != nil && (*t.TaxRateBP < 0 || *t.TaxRateBP > 10000):
		return invalid("invalid_tax_rate", "tax_rate_bp", "Tax is in basis points, from 0 to 10000. For 8.25%, send 825.")
	case t.MinPerOrder < 1 || t.MinPerOrder > 100:
		return invalid("bad_limits", "min_per_order", "Per-order limits run from 1 to 100, and the minimum can't be above the maximum.")
	case t.MaxPerOrder < 1 || t.MaxPerOrder > 100 || t.MinPerOrder > t.MaxPerOrder:
		return invalid("bad_limits", "max_per_order", "Per-order limits run from 1 to 100, and the minimum can't be above the maximum.")
	case t.SalesStart != nil && t.SalesEnd != nil && !t.SalesEnd.After(*t.SalesStart):
		return invalid("bad_sales_window", "sales_end", "Sales have to end after they start.")
	}
	return nil
}

// ttDBError is dbError for ticket types: 23514 is a failed CHECK, 23503 a
// hold or ticket still pointing at it (a sale the guards missed).
func ttDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{Code: "invalid_ticket_type", Message: "Something in that ticket type isn't allowed. Check the lengths, price, limits and dates."}
		case "23505":
			return ErrConflict{"ticket_type_exists", "", "That ticket type already exists."}
		case "23503":
			return errHasSales("", "this ticket")
		}
	}
	return err
}

// lockEvent locks the scoped organizer's event row, so child writes (ticket
// types, quotas, promo codes, questions, reorders) line up one behind the other
// and behind going live. Another tenant's event is ErrNotFound.
//
// FOR NO KEY UPDATE, not FOR UPDATE: the child writes never change the event's
// keys, and checkouts, holds and orders take FOR KEY SHARE on the event through
// their foreign keys, which FOR UPDATE would block. UpdateEvent uses it too (a
// slug change upgrades the lock as it writes); DeleteEvent keeps FOR UPDATE,
// since a delete removes the row and only drafts can go.
func lockEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Event, error) {
	return scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1 for no key update`, id))
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
	ev, err := lockEvent(ctx, tx, event)
	if err != nil {
		return TicketType{}, err
	}
	quotas := dedupe(in.QuotaIDs)
	if ev.Status == "live" && len(quotas) == 0 {
		return TicketType{}, ErrConflict{"live_needs_capacity", "quota_ids", "Every ticket on a live event needs a capacity. Move the event back to draft first, or pass quota_ids."}
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from quotas where event_id = $1 and id = any($2)`, event, quotas).Scan(&n); err != nil {
		return TicketType{}, err
	}
	if n != len(quotas) {
		return TicketType{}, invalid("unknown_quota", "quota_ids", "One of those quotas isn't part of this event.")
	}
	t, err = scanTT(tx.QueryRow(ctx, `insert into ticket_types(organizer_id, event_id, name, description, price_cents, tax_rate_bp, prices_include_tax,
			sales_start, sales_end, min_per_order, max_per_order, hidden, position)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			(select coalesce(max(position), 0) + 1 from ticket_types where event_id = $2)) returning `+ttCols,
		org, event, t.Name, t.Description, t.PriceCents, t.TaxRateBP, t.PricesIncludeTax, t.SalesStart, t.SalesEnd, t.MinPerOrder, t.MaxPerOrder, t.Hidden))
	if err != nil {
		return TicketType{}, ttDBError(err)
	}
	if err := link(ctx, tx, org, event, quotas, []uuid.UUID{t.ID}); err != nil {
		return TicketType{}, err
	}
	after := struct {
		TicketType
		QuotaIDs []uuid.UUID `json:"quota_ids"`
	}{t, quotas}
	return t, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.created", EntityType: "ticket_type", EntityID: t.ID, After: after})
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
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return TicketType{}, err
	}
	// No row lock: the event lock keeps other catalogue writes off this row,
	// and one order everywhere (event, then quotas, then rows) keeps the
	// guard's quota locks ahead of the row lock the update takes.
	before, err := GetTicketType(ctx, tx, event, id)
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
	if before.Hidden && !t.Hidden {
		var code string
		err := tx.QueryRow(ctx, `select p.code from promo_unlocks u join promo_codes p on p.id = u.promo_code_id
			where u.ticket_type_id = $1 order by p.code limit 1`, id).Scan(&code)
		if err == nil {
			return TicketType{}, ErrConflict{"unlocked_by_promo", "", "Remove it from promo code " + code + " first, or keep it hidden."}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return TicketType{}, err
		}
	}
	if field := pricingChange(t, before); field != "" {
		if err := guardSales(ctx, tx, event, []uuid.UUID{id}, field, "this ticket"); err != nil {
			return TicketType{}, err
		}
	}
	t, err = scanTT(tx.QueryRow(ctx, `update ticket_types set name = $2, description = $3, price_cents = $4, tax_rate_bp = $5, prices_include_tax = $6,
			sales_start = $7, sales_end = $8, min_per_order = $9, max_per_order = $10, hidden = $11, updated_at = clock_timestamp()
		where id = $1 returning `+ttCols,
		id, t.Name, t.Description, t.PriceCents, t.TaxRateBP, t.PricesIncludeTax, t.SalesStart, t.SalesEnd, t.MinPerOrder, t.MaxPerOrder, t.Hidden))
	if err != nil {
		return TicketType{}, ttDBError(err)
	}
	return t, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.updated", EntityType: "ticket_type", EntityID: id, Before: before, After: t})
}

// pricingChange names the first field a buyer was quoted on that differs:
// price, tax and per-order limits. "" when none does.
func pricingChange(a, b TicketType) string {
	switch {
	case a.PriceCents != b.PriceCents:
		return "price_cents"
	case !samePtr(a.TaxRateBP, b.TaxRateBP):
		return "tax_rate_bp"
	case !samePtr(a.PricesIncludeTax, b.PricesIncludeTax):
		return "prices_include_tax"
	case a.MinPerOrder != b.MinPerOrder:
		return "min_per_order"
	case a.MaxPerOrder != b.MaxPerOrder:
		return "max_per_order"
	}
	return ""
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
	// Read before locking: the quotas come first (lock order), and the event
	// lock already keeps other catalogue writes off this row.
	before, err := GetTicketType(ctx, tx, event, id)
	if err != nil {
		return err
	}
	if err := guardSales(ctx, tx, event, []uuid.UUID{id}, "", "this ticket"); err != nil {
		return err
	}
	if ev.Status == "live" {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from ticket_types where event_id = $1`, event).Scan(&n); err != nil {
			return err
		}
		if n == 1 {
			return ErrConflict{"last_ticket_type_live", "", "Move the event back to draft before removing its last ticket type."}
		}
	}
	var quotas []uuid.UUID
	if err := tx.QueryRow(ctx, `select coalesce(array_agg(quota_id order by quota_id), '{}') from quota_ticket_types where ticket_type_id = $1`, id).Scan(&quotas); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from ticket_types where id = $1`, id); err != nil {
		return ttDBError(err)
	}
	// Before lists the quotas it was in, as ticket_type.created's After does.
	b := struct {
		TicketType
		QuotaIDs []uuid.UUID `json:"quota_ids"`
	}{before, quotas}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket_type.deleted", EntityType: "ticket_type", EntityID: id, Before: b})
}

// ReorderTicketTypes gives the event's ticket types positions 1..n in the
// order of ids, which must be exactly the current set, and records
// ticket_type.reordered. The same order again writes nothing.
func ReorderTicketTypes(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, ids []uuid.UUID) ([]TicketType, error) {
	if err := reorder(ctx, tx, org, event, actor, "ticket_types", "ticket_type.reordered", ids); err != nil {
		return nil, err
	}
	return ListTicketTypes(ctx, tx, event)
}
