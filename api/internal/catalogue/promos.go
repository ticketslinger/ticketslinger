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

// PromoCode is a discount a buyer types in. AppliesTo empty means every
// ticket type; Unlocks lists the hidden ticket types the code reveals.
// UsedCount is read-only here: checkout (P3) counts it.
type PromoCode struct {
	ID         uuid.UUID   `json:"id"`
	EventID    uuid.UUID   `json:"event_id"`
	Code       string      `json:"code"`
	Kind       string      `json:"kind"`  // percent | amount
	Value      int64       `json:"value"` // basis points for percent, cents for amount
	MaxUses    *int        `json:"max_uses"`
	UsedCount  int         `json:"used_count"`
	ValidFrom  *time.Time  `json:"valid_from"`
	ValidUntil *time.Time  `json:"valid_until"`
	AppliesTo  []uuid.UUID `json:"applies_to"`
	Unlocks    []uuid.UUID `json:"unlocks"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

type PromoInput struct {
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
	Value              *int64
	MaxUses            nullable.Nullable[int]
	ValidFrom          nullable.Nullable[time.Time]
	ValidUntil         nullable.Nullable[time.Time]
	AppliesTo, Unlocks *[]uuid.UUID
}

const promoCols = `id, event_id, code, kind, value, max_uses, used_count, valid_from, valid_until,
	coalesce((select array_agg(ticket_type_id order by ticket_type_id) from promo_applies_to where promo_code_id = promo_codes.id), '{}'),
	coalesce((select array_agg(ticket_type_id order by ticket_type_id) from promo_unlocks where promo_code_id = promo_codes.id), '{}'),
	created_at, updated_at`

func scanPromo(row pgx.Row) (PromoCode, error) {
	var p PromoCode
	err := row.Scan(&p.ID, &p.EventID, &p.Code, &p.Kind, &p.Value, &p.MaxUses, &p.UsedCount, &p.ValidFrom, &p.ValidUntil,
		&p.AppliesTo, &p.Unlocks, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoCode{}, ErrNotFound
	}
	return p, err
}

var promoCodeRE = regexp.MustCompile(`^[A-Z0-9-]{3,32}$`)

// validatePromo holds the rules the CHECK constraints in 00010 back up. The
// code is already trimmed and upper-cased.
func validatePromo(p *PromoCode) error {
	switch {
	case !promoCodeRE.MatchString(p.Code):
		return invalid("bad_code", "Use 3 to 32 letters, numbers and hyphens.")
	case p.Kind != "percent" && p.Kind != "amount":
		return invalid("bad_kind", "A discount is either a percent or an amount.")
	case p.Kind == "percent" && (p.Value < 1 || p.Value > 10000):
		return invalid("bad_discount", "A percent discount runs from 0.01% to 100%. Send it in basis points: 10% is 1000.")
	case p.Kind == "amount" && (p.Value < 1 || p.Value > 10000000):
		return invalid("bad_discount", "An amount discount runs from 1 cent to $100,000. Send it in cents: $5 is 500.")
	case p.MaxUses != nil && *p.MaxUses <= 0:
		return invalid("bad_max_uses", "A code that can be used at most 0 times is no use. Send 1 or more, or null for no limit.")
	case p.ValidFrom != nil && p.ValidUntil != nil && !p.ValidUntil.After(*p.ValidFrom):
		return invalid("bad_validity", "The code has to stop working after it starts.")
	}
	return nil
}

func normCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// promoDBError is dbError for promo codes: 23514 a failed CHECK, 23505 the
// per-event code.
func promoDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{"invalid_promo", "Something in that promo code isn't allowed. Check the code, the discount and the dates."}
		case "23505":
			return ErrConflict{"code_taken", "This event already has that code. Pick another."}
		}
	}
	return err
}

// promoSets checks, inside the locked transaction, that every id is a ticket
// type of this event and that unlocks are all hidden.
func promoSets(ctx context.Context, tx pgx.Tx, event uuid.UUID, applies, unlocks []uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	applies, err := linkSet(ctx, tx, event, applies)
	if err != nil {
		return nil, nil, err
	}
	if unlocks, err = linkSet(ctx, tx, event, unlocks); err != nil {
		return nil, nil, err
	}
	var name string
	err = tx.QueryRow(ctx, `select name from ticket_types where event_id = $1 and id = any($2) and not hidden order by position, id limit 1`, event, unlocks).Scan(&name)
	if err == nil {
		return nil, nil, invalid("unlock_not_hidden", "“"+name+"” isn't hidden, so there's nothing to unlock. Hide it first, or leave it out.")
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
	p := PromoCode{Code: normCode(in.Code), Kind: in.Kind, Value: in.Value, MaxUses: in.MaxUses, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil}
	if err := validatePromo(&p); err != nil {
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
	if err := tx.QueryRow(ctx, `insert into promo_codes(organizer_id, event_id, code, kind, value, max_uses, valid_from, valid_until)
		values ($1, $2, $3, $4, $5, $6, $7, $8) returning id`,
		org, event, p.Code, p.Kind, p.Value, p.MaxUses, p.ValidFrom, p.ValidUntil).Scan(&id); err != nil {
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
		p.Code = normCode(*patch.Code)
	}
	if patch.Kind != nil {
		p.Kind = *patch.Kind
	}
	if patch.Value != nil {
		p.Value = *patch.Value
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
	if p.Code == before.Code && p.Kind == before.Kind && p.Value == before.Value && samePtr(p.MaxUses, before.MaxUses) &&
		sameTime(p.ValidFrom, before.ValidFrom) && sameTime(p.ValidUntil, before.ValidUntil) &&
		slices.Equal(p.AppliesTo, before.AppliesTo) && slices.Equal(p.Unlocks, before.Unlocks) {
		return before, nil
	}
	if _, err := tx.Exec(ctx, `update promo_codes set code = $2, kind = $3, value = $4, max_uses = $5, valid_from = $6, valid_until = $7,
		updated_at = clock_timestamp() where id = $1`, id, p.Code, p.Kind, p.Value, p.MaxUses, p.ValidFrom, p.ValidUntil); err != nil {
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

// DeletePromo removes a code and records promo_code.deleted.
func DeletePromo(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor) error {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return err
	}
	before, err := scanPromo(tx.QueryRow(ctx, `select `+promoCols+` from promo_codes where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return err
	}
	// ponytail: P3 must guard this once orders have redeemed the code.
	if _, err := tx.Exec(ctx, `delete from promo_codes where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "promo_code.deleted", EntityType: "promo_code", EntityID: id, Before: before})
}
