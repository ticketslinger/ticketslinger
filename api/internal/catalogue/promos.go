package catalogue

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

// PromoCode is a discount a buyer types in. AppliesToAll means every ticket
// type; otherwise only those in AppliesTo (empty then means none, e.g. after
// they were deleted, never everything). Unlocks lists the hidden ticket types the code reveals.
// UsedCount is read-only here: checkout (P3) counts it.
type PromoCode struct {
	ID           uuid.UUID   `json:"id"`
	EventID      uuid.UUID   `json:"event_id"`
	Code         string      `json:"code"`
	Kind         string      `json:"kind"`  // percent | amount
	Value        int64       `json:"value"` // basis points for percent, cents for amount
	MaxUses      *int        `json:"max_uses"`
	UsedCount    int         `json:"used_count"`
	ValidFrom    *time.Time  `json:"valid_from"`
	ValidUntil   *time.Time  `json:"valid_until"`
	AppliesToAll bool        `json:"applies_to_all"`
	AppliesTo    []uuid.UUID `json:"applies_to"`
	Unlocks      []uuid.UUID `json:"unlocks"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type PromoInput struct {
	AppliesToAll          *bool // nil means true
	Code, Kind            string
	Value                 int64
	MaxUses               *int
	ValidFrom, ValidUntil *time.Time
	AppliesTo, Unlocks    []uuid.UUID
}

// PromoPatch changes only what is set. Nil AppliesTo or Unlocks leaves the
// set alone; a non-nil one (even empty) replaces it. For the nullable fields
// absent keeps and an explicit null clears.
type PromoPatch struct {
	Code, Kind         *string
	AppliesToAll       *bool
	Value              *int64
	MaxUses            nullable.Nullable[int]
	ValidFrom          nullable.Nullable[time.Time]
	ValidUntil         nullable.Nullable[time.Time]
	AppliesTo, Unlocks *[]uuid.UUID
}

const promoCols = `id, event_id, code, kind, value, max_uses, used_count, valid_from, valid_until, applies_to_all,
	coalesce((select array_agg(ticket_type_id order by ticket_type_id) from promo_applies_to where promo_code_id = promo_codes.id), '{}'),
	coalesce((select array_agg(ticket_type_id order by ticket_type_id) from promo_unlocks where promo_code_id = promo_codes.id), '{}'),
	created_at, updated_at`

func scanPromo(row pgx.Row) (PromoCode, error) {
	var p PromoCode
	err := row.Scan(&p.ID, &p.EventID, &p.Code, &p.Kind, &p.Value, &p.MaxUses, &p.UsedCount, &p.ValidFrom, &p.ValidUntil, &p.AppliesToAll,
		&p.AppliesTo, &p.Unlocks, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoCode{}, ErrNotFound
	}
	return p, err
}

var promoCodeRE = regexp.MustCompile(`^[A-Z0-9-]{3,32}$`)

// validatePromo holds the rules the CHECK constraints in 00010 and 00015 back
// up. The code is already trimmed and upper-cased. A value of 0 is allowed
// here; accessOnly decides whether it may stand.
func validatePromo(p *PromoCode) error {
	switch {
	case !promoCodeRE.MatchString(p.Code):
		return invalid("bad_code", "code", "Use 3 to 32 letters, numbers and hyphens.")
	case p.Kind != "percent" && p.Kind != "amount":
		return invalid("bad_kind", "kind", "A discount is either a percent or an amount.")
	case p.Kind == "percent" && (p.Value < 0 || p.Value > 10000):
		return invalid("bad_discount", "value", "A percent discount runs from 0% to 100%. Send it in basis points: 10% is 1000.")
	case p.Kind == "amount" && (p.Value < 0 || p.Value > 10000000):
		return invalid("bad_discount", "value", "An amount discount runs from $0 to $100,000. Send it in cents: $5 is 500.")
	case p.MaxUses != nil && *p.MaxUses <= 0:
		return invalid("bad_max_uses", "max_uses", "A code that can be used at most 0 times is no use. Send 1 or more, or null for no limit.")
	case p.ValidFrom != nil && p.ValidUntil != nil && !p.ValidUntil.After(*p.ValidFrom):
		return invalid("bad_validity", "valid_until", "The code has to stop working after it starts.")
	}
	return nil
}

// accessOnly: a code worth 0 takes nothing off, so it has to reveal at least
// one hidden ticket (an access code at full price) or it does nothing at all.
func accessOnly(value int64, unlocks []uuid.UUID) error {
	if value == 0 && len(unlocks) == 0 {
		return invalid("bad_discount", "value", "A code with no discount has to show at least one hidden ticket. Pick one to unlock, or set a discount.")
	}
	return nil
}

// NormalizePromoCode is the one way to turn what a person typed into a stored
// code. Checkout must use it for every lookup, or SPRING won't find spring.
func NormalizePromoCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// validateAppliesTo: all-tickets and a list are mutually exclusive, and a list
// can't be empty.
func validateAppliesTo(all bool, applies []uuid.UUID) error {
	switch {
	case all && len(applies) > 0:
		return invalid("applies_to_conflict", "applies_to", "A code for all tickets can't also list ticket types. Send applies_to_all: false to limit it, or leave applies_to empty.")
	case !all && len(applies) == 0:
		return invalid("applies_to_required", "applies_to", "Pick at least one ticket type, or set applies_to_all to true.")
	}
	return nil
}

// promoDBError is dbError for promo codes: 23514 a failed CHECK, 23505 the
// per-event code, 23503 a link whose ticket type isn't there (or isn't in this
// event).
func promoDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{Code: "invalid_promo", Message: "Something in that promo code isn't allowed. Check the code, the discount and the dates."}
		case "23503":
			return ErrInvalid{Code: "invalid_promo", Message: "One of those ticket types isn't part of this event any more. Reload and try again."}
		case "23505":
			return ErrConflict{"code_taken", "code", "This event already has that code. Pick another."}
		}
	}
	return err
}

// promoSets checks, inside the locked transaction, that every id is a ticket
// type of this event and that unlocks are all hidden.
func promoSets(ctx context.Context, tx pgx.Tx, event uuid.UUID, applies, unlocks []uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	applies, err := linkSet(ctx, tx, event, applies, "applies_to")
	if err != nil {
		return nil, nil, err
	}
	if unlocks, err = linkSet(ctx, tx, event, unlocks, "unlocks"); err != nil {
		return nil, nil, err
	}
	var name string
	err = tx.QueryRow(ctx, `select name from ticket_types where event_id = $1 and id = any($2) and not hidden order by position, id limit 1`, event, unlocks).Scan(&name)
	if err == nil {
		return nil, nil, invalid("unlock_not_hidden", "unlocks", "“"+name+"” isn't hidden, so there's nothing to unlock. Hide it first, or leave it out.")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	return applies, unlocks, nil
}

// setPromoLinks replaces both sets of a code; the ids are already checked.
func setPromoLinks(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, applies, unlocks []uuid.UUID) error {
	for table, ids := range map[string][]uuid.UUID{"promo_applies_to": applies, "promo_unlocks": unlocks} {
		if _, err := tx.Exec(ctx, `delete from `+table+` where promo_code_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into `+table+`(organizer_id, event_id, promo_code_id, ticket_type_id)
			select $1, $2, $3, t from unnest($4::uuid[]) t`, org, event, id, ids); err != nil {
			return promoDBError(err)
		}
	}
	return nil
}

// CreatePromo adds a code and records promo_code.created by actor. tx must be
// scoped to org.
func CreatePromo(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, in PromoInput) (PromoCode, error) {
	p := PromoCode{Code: NormalizePromoCode(in.Code), Kind: in.Kind, Value: in.Value, MaxUses: in.MaxUses, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil}
	p.AppliesToAll = in.AppliesToAll == nil || *in.AppliesToAll
	if err := validatePromo(&p); err != nil {
		return PromoCode{}, err
	}
	if err := validateAppliesTo(p.AppliesToAll, dedupe(in.AppliesTo)); err != nil {
		return PromoCode{}, err
	}
	if err := accessOnly(p.Value, in.Unlocks); err != nil {
		return PromoCode{}, err
	}
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return PromoCode{}, err
	}
	applies, unlocks, err := promoSets(ctx, tx, event, in.AppliesTo, in.Unlocks)
	if err != nil {
		return PromoCode{}, err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into promo_codes(organizer_id, event_id, code, kind, value, max_uses, valid_from, valid_until, applies_to_all)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9) returning id`,
		org, event, p.Code, p.Kind, p.Value, p.MaxUses, p.ValidFrom, p.ValidUntil, p.AppliesToAll).Scan(&id); err != nil {
		return PromoCode{}, promoDBError(err)
	}
	if err := setPromoLinks(ctx, tx, org, event, id, applies, unlocks); err != nil {
		return PromoCode{}, err
	}
	if p, err = GetPromo(ctx, tx, event, id); err != nil {
		return PromoCode{}, err
	}
	return p, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "promo_code.created", EntityType: "promo_code", EntityID: p.ID, After: p})
}

func GetPromo(ctx context.Context, tx pgx.Tx, event, id uuid.UUID) (PromoCode, error) {
	return scanPromo(tx.QueryRow(ctx, `select `+promoCols+` from promo_codes where id = $1 and event_id = $2`, id, event))
}

// ListPromos returns the event's codes, oldest first.
func ListPromos(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]PromoCode, error) {
	if _, err := GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select `+promoCols+` from promo_codes where event_id = $1 order by created_at, id`, event)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PromoCode, error) { return scanPromo(r) })
}

func samePtr[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// UpdatePromo applies patch under the event lock and records
// promo_code.updated. A patch that changes nothing writes nothing.
func UpdatePromo(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor, patch PromoPatch) (PromoCode, error) {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return PromoCode{}, err
	}
	before, err := scanPromo(tx.QueryRow(ctx, `select `+promoCols+` from promo_codes where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return PromoCode{}, err
	}
	p := before
	if patch.Code != nil {
		p.Code = NormalizePromoCode(*patch.Code)
	}
	if patch.Kind != nil {
		p.Kind = *patch.Kind
	}
	if patch.Value != nil {
		p.Value = *patch.Value
	}
	if patch.Kind != nil && *patch.Kind != before.Kind && patch.Value == nil {
		return PromoCode{}, invalid("value_required_with_kind", "value", "Set the amount again when you switch between a percentage and a fixed amount.")
	}
	if patch.AppliesToAll != nil {
		p.AppliesToAll = *patch.AppliesToAll
	}
	setNull(&p.MaxUses, patch.MaxUses)
	setNull(&p.ValidFrom, patch.ValidFrom)
	setNull(&p.ValidUntil, patch.ValidUntil)
	if err := validatePromo(&p); err != nil {
		return PromoCode{}, err
	}
	if patch.AppliesTo != nil || patch.Unlocks != nil {
		applies, unlocks := p.AppliesTo, p.Unlocks
		if patch.AppliesTo != nil {
			applies = *patch.AppliesTo
		}
		if patch.Unlocks != nil {
			unlocks = *patch.Unlocks
		}
		if p.AppliesTo, p.Unlocks, err = promoSets(ctx, tx, event, applies, unlocks); err != nil {
			return PromoCode{}, err
		}
	}
	// Only when the patch touches the scope or the discount: a code emptied
	// by a deleted ticket type can still have its other fields edited.
	if patch.AppliesTo != nil || patch.AppliesToAll != nil {
		if err := validateAppliesTo(p.AppliesToAll, p.AppliesTo); err != nil {
			return PromoCode{}, err
		}
	}
	if patch.Value != nil || patch.Unlocks != nil {
		if err := accessOnly(p.Value, p.Unlocks); err != nil {
			return PromoCode{}, err
		}
	}
	if p.Code == before.Code && p.Kind == before.Kind && p.Value == before.Value && samePtr(p.MaxUses, before.MaxUses) && p.AppliesToAll == before.AppliesToAll &&
		sameTime(p.ValidFrom, before.ValidFrom) && sameTime(p.ValidUntil, before.ValidUntil) &&
		slices.Equal(p.AppliesTo, before.AppliesTo) && slices.Equal(p.Unlocks, before.Unlocks) {
		return before, nil
	}
	// What the code takes off, and from which tickets, is fixed once used.
	if field := promoTermsChange(p, before); field != "" {
		if err := promoUsed(ctx, tx, id, field); err != nil {
			return PromoCode{}, err
		}
	}
	if _, err := tx.Exec(ctx, `update promo_codes set code = $2, kind = $3, value = $4, max_uses = $5, valid_from = $6, valid_until = $7,
		applies_to_all = $8, updated_at = clock_timestamp() where id = $1`, id, p.Code, p.Kind, p.Value, p.MaxUses, p.ValidFrom, p.ValidUntil, p.AppliesToAll); err != nil {
		return PromoCode{}, promoDBError(err)
	}
	if err := setPromoLinks(ctx, tx, org, event, id, p.AppliesTo, p.Unlocks); err != nil {
		return PromoCode{}, err
	}
	if p, err = GetPromo(ctx, tx, event, id); err != nil {
		return PromoCode{}, err
	}
	return p, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "promo_code.updated", EntityType: "promo_code", EntityID: id, Before: before, After: p})
}

// promoTermsChange names the first changed field a buyer's discount depends
// on, "" when none did.
func promoTermsChange(p, before PromoCode) string {
	switch {
	case p.Kind != before.Kind:
		return "kind"
	case p.Value != before.Value:
		return "value"
	case p.AppliesToAll != before.AppliesToAll:
		return "applies_to_all"
	case !slices.Equal(p.AppliesTo, before.AppliesTo):
		return "applies_to"
	}
	return ""
}

// promoUsed refuses changing what a code takes off, or deleting it, once an
// order has redeemed it or an open checkout is using it. The caller holds the
// code's row lock, which a checkout applying it takes too.
func promoUsed(ctx context.Context, tx pgx.Tx, id uuid.UUID, field string) error {
	var used bool
	if err := tx.QueryRow(ctx, `select used_count > 0 or exists (select 1 from checkouts c where c.promo_code_id = $1
		and c.status in ('open', 'paying') and c.expires_at > now()) from promo_codes where id = $1`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return errHasSales(field, "with this code")
	}
	return nil
}

// DeletePromo removes a code and records promo_code.deleted.
func DeletePromo(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor) error {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return err
	}
	before, err := scanPromo(tx.QueryRow(ctx, `select `+promoCols+` from promo_codes where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return err
	}
	if err := promoUsed(ctx, tx, id, ""); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from promo_codes where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "promo_code.deleted", EntityType: "promo_code", EntityID: id, Before: before})
}
