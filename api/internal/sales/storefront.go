package sales

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// StorefrontEvent is a live event as a buyer sees it, with what one promo
// code (if any) shows them.
type StorefrontEvent struct {
	Organizer   StorefrontOrganizer
	Event       StorefrontEventInfo
	TicketTypes []StorefrontTicketType // in position order
	Promo       *StorefrontPromo       // only for a code that works right now
	Questions   []StorefrontQuestion   // in position order
}

type StorefrontOrganizer struct{ Name, Slug string }

type StorefrontEventInfo struct {
	ID                                                         uuid.UUID
	Slug, Name, Description, VenueName, VenueAddress, Timezone string
	StartsAt, EndsAt                                           time.Time
	FeeMode                                                    string
	State                                                      string // "on_sale" | "ended"
}

type StorefrontTicketType struct {
	ID                                  uuid.UUID
	Name, Description                   string
	PriceCents                          int64
	MinPerOrder, MaxPerOrder, Remaining int
	OnSale                              bool
	SalesStart, SalesEnd                *time.Time
	Unlocked                            bool // a hidden type the code shows
}

type StorefrontPromo struct {
	Code, Kind string
	Value      int64
}

type StorefrontQuestion struct {
	ID                     uuid.UUID
	Label, Kind, Scope     string
	Options                []string
	Required, AppliesToAll bool
	AppliesTo              []uuid.UUID // only the types this buyer can see
}

// StorefrontEvent reads a live event by its storefront address. Hidden
// ticket types, and questions asked only of hidden types, appear only when
// code unlocks them. An unknown or draft event is ErrNotFound, a suspended
// organizer's is ErrUnavailable; a code that doesn't work is simply no code.
func (s *Service) StorefrontEvent(ctx context.Context, org, event, code string) (StorefrontEvent, error) {
	orgID, eventID, err := s.resolveLive(ctx, org, event)
	if err != nil {
		return StorefrontEvent{}, err
	}
	var out StorefrontEvent
	// The system role sees hidden types and promo codes; every query below
	// names what a buyer may see.
	err = system.Run(ctx, s.Pool, orgID, func(tx pgx.Tx) error {
		if err := offSale(ctx, tx, orgID); err != nil {
			return err
		}
		e := &out.Event
		var running bool
		err := tx.QueryRow(ctx, `select o.name, o.slug::text, e.id, e.slug, e.name, e.description, e.venue_name, e.venue_address,
				e.timezone, e.starts_at, e.ends_at, e.fee_mode, statement_timestamp() < e.ends_at
			from events e join organizers o on o.id = e.organizer_id where e.id = $1 and e.status = 'live'`, eventID).
			Scan(&out.Organizer.Name, &out.Organizer.Slug, &e.ID, &e.Slug, &e.Name, &e.Description, &e.VenueName, &e.VenueAddress,
				&e.Timezone, &e.StartsAt, &e.EndsAt, &e.FeeMode, &running)
		if errors.Is(err, pgx.ErrNoRows) {
			return catalogue.ErrNotFound // unpublished since resolveLive looked
		}
		if err != nil {
			return err
		}
		e.State = "ended"
		if running {
			e.State = "on_sale"
		}
		var promoID *uuid.UUID
		if code = catalogue.NormalizePromoCode(code); code != "" {
			var p StorefrontPromo
			var id uuid.UUID
			err := tx.QueryRow(ctx, `select id, code, kind, value from promo_codes where event_id = $1 and code = $2
				and (valid_from is null or valid_from <= statement_timestamp()) and (valid_until is null or statement_timestamp() < valid_until)
				and (max_uses is null or used_count < max_uses)`, eventID, code).Scan(&id, &p.Code, &p.Kind, &p.Value)
			switch {
			case err == nil:
				out.Promo, promoID = &p, &id
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}
		rows, _ := tx.Query(ctx, `select t.id, t.name, t.description, t.price_cents, t.min_per_order, t.max_per_order, coalesce(a.remaining, 0),
				$3 and a.ticket_type_id is not null and coalesce(t.sales_start <= statement_timestamp(), true) and coalesce(statement_timestamp() < t.sales_end, true),
				t.sales_start, t.sales_end, t.hidden
			from ticket_types t left join event_availability($1) a on a.ticket_type_id = t.id
			where t.event_id = $1 and (not t.hidden or exists (select 1 from promo_unlocks u where u.promo_code_id = $2 and u.ticket_type_id = t.id))
			order by t.position, t.id`, eventID, promoID, running)
		var t StorefrontTicketType
		var visible []uuid.UUID
		if _, err := pgx.ForEachRow(rows, []any{&t.ID, &t.Name, &t.Description, &t.PriceCents, &t.MinPerOrder, &t.MaxPerOrder, &t.Remaining,
			&t.OnSale, &t.SalesStart, &t.SalesEnd, &t.Unlocked}, func() error {
			out.TicketTypes = append(out.TicketTypes, t)
			visible = append(visible, t.ID)
			t = StorefrontTicketType{} // fresh pointers for the next row's times
			return nil
		}); err != nil {
			return err
		}
		out.Questions, err = questionsFor(ctx, tx, eventID, visible)
		return err
	})
	return out, err
}

// questionsFor is the event's questions asked of any of types, in position
// order, with applies_to narrowed to those types.
func questionsFor(ctx context.Context, tx pgx.Tx, event uuid.UUID, types []uuid.UUID) ([]StorefrontQuestion, error) {
	rows, _ := tx.Query(ctx, `select q.id, q.label, q.kind, q.scope,
			array(select o from jsonb_array_elements_text(q.options) with ordinality x(o, n) order by n),
			q.required, q.applies_to_all,
			array(select a.ticket_type_id from question_applies_to a where a.question_id = q.id and a.ticket_type_id = any($2) order by a.ticket_type_id)
		from questions q
		where q.event_id = $1 and (q.applies_to_all or exists (select 1 from question_applies_to a where a.question_id = q.id and a.ticket_type_id = any($2)))
		order by q.position, q.id`, event, types)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StorefrontQuestion, error) {
		var q StorefrontQuestion
		err := r.Scan(&q.ID, &q.Label, &q.Kind, &q.Scope, &q.Options, &q.Required, &q.AppliesToAll, &q.AppliesTo)
		return q, err
	})
}
