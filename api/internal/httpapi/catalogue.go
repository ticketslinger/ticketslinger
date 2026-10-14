package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

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
		Name: b.Name, Description: b.Description, VenueName: b.VenueName, VenueAddress: b.VenueAddress, Timezone: b.Timezone,
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
