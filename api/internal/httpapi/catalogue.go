package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

var (
	catalogueRead  = Access{}
	catalogueWrite = Access{Roles: ownerAdmin, Write: true}
)

// actorFrom is whoever made this request, for the audit log: the API key if
// one did, otherwise the signed-in user.
func actorFrom(ctx context.Context) audit.Actor {
	a, _ := ctx.Value(authKey).(authed)
	if a.isKey() {
		return audit.Actor{Key: a.key.KeyID}
	}
	return audit.Actor{User: a.user.ID}
}

// catalogueError maps the catalogue's errors to the API's.
func catalogueError(err error) error {
	var inv catalogue.ErrInvalid
	var con catalogue.ErrConflict
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		return notFound()
	case errors.As(err, &inv):
		return &apiError{422, inv.Code, inv.Message}
	case errors.As(err, &con):
		return &apiError{409, con.Code, con.Message}
	}
	return err
}

func toEvent(e catalogue.Event) Event {
	return Event{
		Id: e.ID, Slug: e.Slug, Name: e.Name, Description: e.Description, VenueName: e.VenueName, VenueAddress: e.VenueAddress,
		Timezone: e.Timezone, StartsAt: e.StartsAt, EndsAt: e.EndsAt, Status: EventStatus(e.Status), FeeMode: EventFeeMode(e.FeeMode),
		Currency: EventCurrency(e.Currency), TaxRateBp: e.TaxRateBP, PricesIncludeTax: e.PricesIncludeTax,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

func (s *server) ListEvents(ctx context.Context, req ListEventsRequestObject) (ListEventsResponseObject, error) {
	out := ListEvents200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ListEvents(ctx, tx)
		for _, e := range list {
			out = append(out, toEvent(e))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) CreateEvent(ctx context.Context, req CreateEventRequestObject) (CreateEventResponseObject, error) {
	b := req.Body
	in := catalogue.EventInput{Slug: b.Slug, Name: b.Name, Timezone: b.Timezone, StartsAt: b.StartsAt, EndsAt: b.EndsAt}
	if b.Description != nil {
		in.Description = *b.Description
	}
	if b.VenueName != nil {
		in.VenueName = *b.VenueName
	}
	if b.VenueAddress != nil {
		in.VenueAddress = *b.VenueAddress
	}
	if b.FeeMode != nil {
		in.FeeMode = string(*b.FeeMode)
	}
	if b.TaxRateBp != nil {
		in.TaxRateBP = *b.TaxRateBp
	}
	if b.PricesIncludeTax != nil {
		in.PricesIncludeTax = *b.PricesIncludeTax
	}
	var out CreateEvent201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		e, err := catalogue.CreateEvent(ctx, tx, org.ID, actorFrom(ctx), in)
		out = CreateEvent201JSONResponse(toEvent(e))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) GetEvent(ctx context.Context, req GetEventRequestObject) (GetEventResponseObject, error) {
	var out GetEvent200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		e, err := catalogue.GetEvent(ctx, tx, req.Event)
		out = GetEvent200JSONResponse(toEvent(e))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) UpdateEvent(ctx context.Context, req UpdateEventRequestObject) (UpdateEventResponseObject, error) {
	b := req.Body
	p := catalogue.EventPatch{
		Slug: b.Slug, Name: b.Name, Description: b.Description, VenueName: b.VenueName, VenueAddress: b.VenueAddress, Timezone: b.Timezone,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, TaxRateBP: b.TaxRateBp, PricesIncludeTax: b.PricesIncludeTax,
	}
	if b.FeeMode != nil {
		v := string(*b.FeeMode)
		p.FeeMode = &v
	}
	if b.Status != nil {
		v := string(*b.Status)
		p.Status = &v
	}
	var out UpdateEvent200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		e, err := catalogue.UpdateEvent(ctx, tx, org.ID, req.Event, actorFrom(ctx), p)
		out = UpdateEvent200JSONResponse(toEvent(e))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) DeleteEvent(ctx context.Context, req DeleteEventRequestObject) (DeleteEventResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		return catalogue.DeleteEvent(ctx, tx, org.ID, req.Event, actorFrom(ctx))
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return DeleteEvent204Response{}, nil
}

// orNil is the value of a nullable request field, or nil when it is null
// or absent. Creating has no difference between the two.
func orNil[T any](n nullable.Nullable[T]) *T {
	if v, err := n.Get(); err == nil {
		return &v
	}
	return nil
}

// nullOf is a stored nullable column as the response field: always present,
// null when empty.
func nullOf[T any](p *T) nullable.Nullable[T] {
	if p == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*p)
}

func toTicketType(t catalogue.TicketType) TicketType {
	return TicketType{
		Id: t.ID, EventId: t.EventID, Name: t.Name, Description: t.Description, PriceCents: t.PriceCents, TaxRateBp: nullOf(t.TaxRateBP),
		PricesIncludeTax: nullOf(t.PricesIncludeTax), SalesStart: nullOf(t.SalesStart), SalesEnd: nullOf(t.SalesEnd), MinPerOrder: t.MinPerOrder,
		MaxPerOrder: t.MaxPerOrder, Hidden: t.Hidden, Position: t.Position, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func (s *server) ListTicketTypes(ctx context.Context, req ListTicketTypesRequestObject) (ListTicketTypesResponseObject, error) {
	out := ListTicketTypes200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ListTicketTypes(ctx, tx, req.Event)
		for _, t := range list {
			out = append(out, toTicketType(t))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) CreateTicketType(ctx context.Context, req CreateTicketTypeRequestObject) (CreateTicketTypeResponseObject, error) {
	b := req.Body
	in := catalogue.TicketTypeInput{Name: b.Name, PriceCents: b.PriceCents, TaxRateBP: orNil(b.TaxRateBp), PricesIncludeTax: orNil(b.PricesIncludeTax),
		SalesStart: orNil(b.SalesStart), SalesEnd: orNil(b.SalesEnd), MinPerOrder: b.MinPerOrder, MaxPerOrder: b.MaxPerOrder}
	if b.Description != nil {
		in.Description = *b.Description
	}
	if b.Hidden != nil {
		in.Hidden = *b.Hidden
	}
	var out CreateTicketType201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		t, err := catalogue.CreateTicketType(ctx, tx, org.ID, req.Event, actorFrom(ctx), in)
		out = CreateTicketType201JSONResponse(toTicketType(t))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) GetTicketType(ctx context.Context, req GetTicketTypeRequestObject) (GetTicketTypeResponseObject, error) {
	var out GetTicketType200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		t, err := catalogue.GetTicketType(ctx, tx, req.Event, req.TicketType)
		out = GetTicketType200JSONResponse(toTicketType(t))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) UpdateTicketType(ctx context.Context, req UpdateTicketTypeRequestObject) (UpdateTicketTypeResponseObject, error) {
	b := req.Body
	p := catalogue.TicketTypePatch{Name: b.Name, Description: b.Description, PriceCents: b.PriceCents, TaxRateBP: b.TaxRateBp,
		PricesIncludeTax: b.PricesIncludeTax, SalesStart: b.SalesStart, SalesEnd: b.SalesEnd, MinPerOrder: b.MinPerOrder,
		MaxPerOrder: b.MaxPerOrder, Hidden: b.Hidden}
	var out UpdateTicketType200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		t, err := catalogue.UpdateTicketType(ctx, tx, org.ID, req.Event, req.TicketType, actorFrom(ctx), p)
		out = UpdateTicketType200JSONResponse(toTicketType(t))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) DeleteTicketType(ctx context.Context, req DeleteTicketTypeRequestObject) (DeleteTicketTypeResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, org.ID, req.Event, req.TicketType, actorFrom(ctx))
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return DeleteTicketType204Response{}, nil
}
