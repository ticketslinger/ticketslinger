package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
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
		if inv.Field != "" {
			return invalidField(inv.Code, inv.Field, inv.Message)
		}
		return &apiError{422, inv.Code, inv.Message}
	case errors.As(err, &con):
		if con.Field != "" {
			return fieldError{&apiError{409, con.Code, con.Message}, con.Field}
		}
		return &apiError{409, con.Code, con.Message}
	}
	return err
}

// missingField is a required number the body left out. Without it a 0 would
// quietly become a free ticket or an empty capacity.
func missingField(name, what string) error {
	return invalidField("missing_field", name, "Send \u201c"+name+"\u201d: "+what+".")
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
		// Checked after membership, so a stranger still gets 404. A missing
		// time decodes as the zero time, year 1.
		switch {
		case b.StartsAt.IsZero():
			return missingField("starts_at", "when the event starts, like 2027-03-01T19:00:00Z")
		case b.EndsAt.IsZero():
			return missingField("ends_at", "when the event ends, like 2027-03-01T22:00:00Z")
		}
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
		mode := catalogue.PaymentsConnect
		if s.direct() {
			mode = catalogue.PaymentsDirect
		}
		e, err := catalogue.UpdateEvent(ctx, tx, org.ID, req.Event, actorFrom(ctx), p, mode)
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

// toTicketType prices all_in_cents with fee, the fee the checkout charges.
func toTicketType(t catalogue.TicketType, fee pricing.Fee) TicketType {
	shown := pricing.ShownPrice(pricing.Line{TicketTypeID: t.ID, Quantity: 1, PriceCents: t.PriceCents,
		TaxRateBP: t.ResolvedTaxBP, PricesIncludeTax: t.ResolvedIncludesTax}, t.FeeMode, fee)
	return TicketType{AllInCents: shown.AllInCents,
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
			out = append(out, toTicketType(t, s.Sales.PlatformFee()))
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
	in := catalogue.TicketTypeInput{Name: b.Name, TaxRateBP: orNil(b.TaxRateBp), PricesIncludeTax: orNil(b.PricesIncludeTax),
		SalesStart: orNil(b.SalesStart), SalesEnd: orNil(b.SalesEnd), MinPerOrder: b.MinPerOrder, MaxPerOrder: b.MaxPerOrder}
	if b.Description != nil {
		in.Description = *b.Description
	}
	if b.Hidden != nil {
		in.Hidden = *b.Hidden
	}
	if b.QuotaIds != nil {
		in.QuotaIDs = *b.QuotaIds
	}
	var out CreateTicketType201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		// Checked after membership, so a stranger still gets 404.
		if b.PriceCents == nil {
			return missingField("price_cents", "the price in cents: $25 is 2500")
		}
		in.PriceCents = *b.PriceCents
		t, err := catalogue.CreateTicketType(ctx, tx, org.ID, req.Event, actorFrom(ctx), in)
		out = CreateTicketType201JSONResponse(toTicketType(t, s.Sales.PlatformFee()))
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
		out = GetTicketType200JSONResponse(toTicketType(t, s.Sales.PlatformFee()))
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
		out = UpdateTicketType200JSONResponse(toTicketType(t, s.Sales.PlatformFee()))
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

func toQuota(q catalogue.Quota) Quota {
	return Quota{Id: q.ID, EventId: q.EventID, Name: q.Name, Size: q.Size, TicketTypeIds: q.TicketTypeIDs, CreatedAt: q.CreatedAt, UpdatedAt: q.UpdatedAt}
}

func (s *server) ListQuotas(ctx context.Context, req ListQuotasRequestObject) (ListQuotasResponseObject, error) {
	out := ListQuotas200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ListQuotas(ctx, tx, req.Event)
		for _, q := range list {
			out = append(out, toQuota(q))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) CreateQuota(ctx context.Context, req CreateQuotaRequestObject) (CreateQuotaResponseObject, error) {
	b := req.Body
	in := catalogue.QuotaInput{Name: b.Name}
	if b.TicketTypeIds != nil {
		in.TicketTypeIDs = *b.TicketTypeIds
	}
	var out CreateQuota201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		if b.Size == nil {
			return missingField("size", "how many tickets the quota holds")
		}
		in.Size = *b.Size
		q, err := catalogue.CreateQuota(ctx, tx, org.ID, req.Event, actorFrom(ctx), in)
		out = CreateQuota201JSONResponse(toQuota(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) GetQuota(ctx context.Context, req GetQuotaRequestObject) (GetQuotaResponseObject, error) {
	var out GetQuota200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		q, err := catalogue.GetQuota(ctx, tx, req.Event, req.Quota)
		out = GetQuota200JSONResponse(toQuota(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) UpdateQuota(ctx context.Context, req UpdateQuotaRequestObject) (UpdateQuotaResponseObject, error) {
	b := req.Body
	p := catalogue.QuotaPatch{Name: b.Name, Size: b.Size, TicketTypeIDs: b.TicketTypeIds}
	var out UpdateQuota200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		q, err := catalogue.UpdateQuota(ctx, tx, org.ID, req.Event, req.Quota, actorFrom(ctx), p)
		out = UpdateQuota200JSONResponse(toQuota(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) DeleteQuota(ctx context.Context, req DeleteQuotaRequestObject) (DeleteQuotaResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		return catalogue.DeleteQuota(ctx, tx, org.ID, req.Event, req.Quota, actorFrom(ctx))
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return DeleteQuota204Response{}, nil
}

func toPromo(p catalogue.PromoCode) PromoCode {
	return PromoCode{Id: p.ID, EventId: p.EventID, Code: p.Code, Kind: PromoCodeKind(p.Kind), Value: p.Value, MaxUses: nullOf(p.MaxUses),
		UsedCount: p.UsedCount, AppliesToAll: p.AppliesToAll, ValidFrom: nullOf(p.ValidFrom), ValidUntil: nullOf(p.ValidUntil), AppliesTo: p.AppliesTo, Unlocks: p.Unlocks,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func (s *server) ListPromoCodes(ctx context.Context, req ListPromoCodesRequestObject) (ListPromoCodesResponseObject, error) {
	out := ListPromoCodes200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ListPromos(ctx, tx, req.Event)
		for _, p := range list {
			out = append(out, toPromo(p))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) CreatePromoCode(ctx context.Context, req CreatePromoCodeRequestObject) (CreatePromoCodeResponseObject, error) {
	b := req.Body
	in := catalogue.PromoInput{AppliesToAll: b.AppliesToAll, MaxUses: orNil(b.MaxUses), ValidFrom: orNil(b.ValidFrom), ValidUntil: orNil(b.ValidUntil)}
	if b.AppliesTo != nil {
		in.AppliesTo = *b.AppliesTo
	}
	if b.Unlocks != nil {
		in.Unlocks = *b.Unlocks
	}
	var out CreatePromoCode201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		// Checked after membership, so a stranger still gets 404.
		switch {
		case b.Code == nil:
			return missingField("code", "what buyers type in, like EARLYBIRD")
		case b.Kind == nil:
			return missingField("kind", "percent or amount")
		case b.Value == nil:
			return missingField("value", "basis points for a percent (1000 is 10%), cents for an amount")
		}
		in.Code, in.Kind, in.Value = *b.Code, *b.Kind, *b.Value
		p, err := catalogue.CreatePromo(ctx, tx, org.ID, req.Event, actorFrom(ctx), in)
		out = CreatePromoCode201JSONResponse(toPromo(p))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) GetPromoCode(ctx context.Context, req GetPromoCodeRequestObject) (GetPromoCodeResponseObject, error) {
	var out GetPromoCode200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		p, err := catalogue.GetPromo(ctx, tx, req.Event, req.PromoCode)
		out = GetPromoCode200JSONResponse(toPromo(p))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) UpdatePromoCode(ctx context.Context, req UpdatePromoCodeRequestObject) (UpdatePromoCodeResponseObject, error) {
	b := req.Body
	p := catalogue.PromoPatch{AppliesToAll: b.AppliesToAll, Code: b.Code, Value: b.Value, MaxUses: b.MaxUses, ValidFrom: b.ValidFrom, ValidUntil: b.ValidUntil,
		AppliesTo: b.AppliesTo, Unlocks: b.Unlocks}
	if b.Kind != nil {
		k := string(*b.Kind)
		p.Kind = &k
	}
	var out UpdatePromoCode200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		x, err := catalogue.UpdatePromo(ctx, tx, org.ID, req.Event, req.PromoCode, actorFrom(ctx), p)
		out = UpdatePromoCode200JSONResponse(toPromo(x))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) DeletePromoCode(ctx context.Context, req DeletePromoCodeRequestObject) (DeletePromoCodeResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		return catalogue.DeletePromo(ctx, tx, org.ID, req.Event, req.PromoCode, actorFrom(ctx))
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return DeletePromoCode204Response{}, nil
}

func toQuestion(q catalogue.Question) Question {
	return Question{Id: q.ID, EventId: q.EventID, Label: q.Label, Kind: QuestionKind(q.Kind), Options: q.Options, Required: q.Required,
		Scope: QuestionScope(q.Scope), AppliesToAll: q.AppliesToAll, AppliesTo: q.AppliesTo, Position: q.Position, CreatedAt: q.CreatedAt, UpdatedAt: q.UpdatedAt}
}

func (s *server) ListQuestions(ctx context.Context, req ListQuestionsRequestObject) (ListQuestionsResponseObject, error) {
	out := ListQuestions200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ListQuestions(ctx, tx, req.Event)
		for _, q := range list {
			out = append(out, toQuestion(q))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) CreateQuestion(ctx context.Context, req CreateQuestionRequestObject) (CreateQuestionResponseObject, error) {
	b := req.Body
	in := catalogue.QuestionInput{AppliesToAll: b.AppliesToAll}
	if b.Options != nil {
		in.Options = *b.Options
	}
	if b.Required != nil {
		in.Required = *b.Required
	}
	if b.AppliesTo != nil {
		in.AppliesTo = *b.AppliesTo
	}
	var out CreateQuestion201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		// Checked after membership, so a stranger still gets 404.
		switch {
		case b.Label == nil:
			return missingField("label", "what the buyer is asked")
		case b.Kind == nil:
			return missingField("kind", "text, select or checkbox")
		case b.Scope == nil:
			return missingField("scope", "order to ask once per order, ticket to ask for every ticket")
		}
		in.Label, in.Kind, in.Scope = *b.Label, *b.Kind, *b.Scope
		q, err := catalogue.CreateQuestion(ctx, tx, org.ID, req.Event, actorFrom(ctx), in)
		out = CreateQuestion201JSONResponse(toQuestion(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) GetQuestion(ctx context.Context, req GetQuestionRequestObject) (GetQuestionResponseObject, error) {
	var out GetQuestion200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		q, err := catalogue.GetQuestion(ctx, tx, req.Event, req.Question)
		out = GetQuestion200JSONResponse(toQuestion(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) UpdateQuestion(ctx context.Context, req UpdateQuestionRequestObject) (UpdateQuestionResponseObject, error) {
	b := req.Body
	p := catalogue.QuestionPatch{Label: b.Label, Options: b.Options, Required: b.Required, AppliesToAll: b.AppliesToAll, AppliesTo: b.AppliesTo}
	if b.Kind != nil {
		v := string(*b.Kind)
		p.Kind = &v
	}
	if b.Scope != nil {
		v := string(*b.Scope)
		p.Scope = &v
	}
	var out UpdateQuestion200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		q, err := catalogue.UpdateQuestion(ctx, tx, org.ID, req.Event, req.Question, actorFrom(ctx), p)
		out = UpdateQuestion200JSONResponse(toQuestion(q))
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) DeleteQuestion(ctx context.Context, req DeleteQuestionRequestObject) (DeleteQuestionResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		return catalogue.DeleteQuestion(ctx, tx, org.ID, req.Event, req.Question, actorFrom(ctx))
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return DeleteQuestion204Response{}, nil
}

func (s *server) ReorderTicketTypes(ctx context.Context, req ReorderTicketTypesRequestObject) (ReorderTicketTypesResponseObject, error) {
	out := ReorderTicketTypes200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ReorderTicketTypes(ctx, tx, org.ID, req.Event, actorFrom(ctx), req.Body.Ids)
		for _, t := range list {
			out = append(out, toTicketType(t, s.Sales.PlatformFee()))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}

func (s *server) ReorderQuestions(ctx context.Context, req ReorderQuestionsRequestObject) (ReorderQuestionsResponseObject, error) {
	out := ReorderQuestions200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		list, err := catalogue.ReorderQuestions(ctx, tx, org.ID, req.Event, actorFrom(ctx), req.Body.Ids)
		for _, q := range list {
			out = append(out, toQuestion(q))
		}
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	return out, nil
}
