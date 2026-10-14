package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/ratelimit"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// providerError logs what the payment provider said and answers 503. The
// provider's errors never carry our keys.
func providerError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "payment provider", "op", op, "err", err)
	return errPaymentsUnavailable
}

func (s *server) paymentsStatus(ctx context.Context, tx pgx.Tx, org organizers.Organizer) (PaymentsStatus, error) {
	st := PaymentsStatus{Provider: PaymentsStatusProviderNone}
	if s.Payments != nil {
		st.Provider = PaymentsStatusProvider(s.Payments.Name())
	}
	err := tx.QueryRow(ctx, `select stripe_account_id is not null, stripe_charges_enabled, stripe_details_submitted from organizers where id = $1`, org.ID).
		Scan(&st.Connected, &st.ChargesEnabled, &st.DetailsSubmitted)
	return st, err
}

func (s *server) GetPayments(ctx context.Context, req GetPaymentsRequestObject) (GetPaymentsResponseObject, error) {
	var out PaymentsStatus
	err := s.inOrganizer(ctx, req.Organizer, Access{}, func(org organizers.Organizer, tx pgx.Tx) (err error) {
		out, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, err
	}
	return GetPayments200JSONResponse(out), nil
}

// ConnectPayments creates the organizer's Stripe account once and hands back
// an onboarding link. The network calls sit inside the transaction on
// purpose: the organizer row lock is what makes two clicks produce one
// account, and this runs once per organizer. The lock is FOR NO KEY UPDATE,
// so foreign-key checks (FOR KEY SHARE) from other writes to this organizer
// don't wait on Stripe. stripe_account_id is a unique column, so writing it
// takes the stronger lock; that happens last, after both Stripe calls.
func (s *server) ConnectPayments(ctx context.Context, req ConnectPaymentsRequestObject) (ConnectPaymentsResponseObject, error) {
	var link string
	var linkErr error
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true, SessionOnly: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		var account *string
		if err := tx.QueryRow(ctx, `select stripe_account_id from organizers where id = $1 for no key update`, org.ID).Scan(&account); err != nil {
			return err
		}
		created := account == nil
		if created {
			id, err := s.Payments.CreateAccount(ctx, "acct-"+org.ID.String())
			if err != nil {
				return providerError(ctx, "create account", err)
			}
			account = &id
		}
		base := s.Config.DashboardOrigin + "/o/" + org.Slug + "/payments"
		// A failed link still commits a new account, so a retry reuses it.
		link, linkErr = s.Payments.OnboardingLink(ctx, *account, base+"?refresh=1", base+"?return=1")
		if !created {
			return nil
		}
		if _, err := tx.Exec(ctx, `update organizers set stripe_account_id = $2 where id = $1`, org.ID, *account); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "organizer.stripe_connected",
			EntityType: "organizer", EntityID: org.ID, After: map[string]string{"stripe_account_id": *account}})
	})
	if err != nil {
		return nil, err
	}
	if linkErr != nil {
		return nil, providerError(ctx, "onboarding link", linkErr)
	}
	return ConnectPayments200JSONResponse{Url: link}, nil
}

// RefreshPayments asks the provider for the account's state and saves it.
func (s *server) RefreshPayments(ctx context.Context, req RefreshPaymentsRequestObject) (RefreshPaymentsResponseObject, error) {
	var out PaymentsStatus
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		// No row lock: only the non-key flags change, and the last write wins.
		var account *string
		if err := tx.QueryRow(ctx, `select stripe_account_id from organizers where id = $1`, org.ID).Scan(&account); err != nil {
			return err
		}
		if account == nil {
			return errNotConnected
		}
		st, err := s.Payments.Account(ctx, *account)
		if err != nil {
			return providerError(ctx, "account", err)
		}
		if _, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2, stripe_details_submitted = $3 where id = $1`,
			org.ID, st.ChargesEnabled, st.DetailsSubmitted); err != nil {
			return err
		}
		out, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, err
	}
	return RefreshPayments200JSONResponse(out), nil
}

// salesError maps the sales package's errors to the API's. sales.ErrBusy
// passes through to writeError (503 try_again with Retry-After).
func salesError(err error) error {
	var so sales.SoldOut
	var done sales.CheckoutCompleted
	switch {
	case errors.As(err, &so):
		available := make([]Availability, len(so.Available))
		for i, a := range so.Available {
			available[i] = Availability{TicketTypeId: a.TicketTypeID, Remaining: a.Remaining}
		}
		return detailError{&apiError{409, "sold_out", "Not enough tickets left for that. Here's what's still available."},
			map[string]any{"available": available}}
	case errors.As(err, &done):
		return detailError{&apiError{409, "checkout_completed", "You've already paid for these. Your order is " + done.OrderCode + ", and the link to your tickets is in your email."},
			map[string]any{"order_code": done.OrderCode}}
	case errors.Is(err, sales.ErrCheckoutExpired):
		return &apiError{410, "checkout_expired", "Your 15 minutes ran out and the tickets went back on sale. Start again and we'll hold them for you."}
	case errors.Is(err, sales.ErrPaymentsNotConfigured):
		return errPaymentsNotConfigured
	case errors.Is(err, sales.ErrPaymentsUnavailable):
		return errPaymentsUnavailable
	}
	return catalogueError(err)
}

var errTooManyTries = &apiError{429, "rate_limited", "That's a lot of tries from one connection. Give it a minute, then try again."}

// allow counts one request from the caller's address against l.
func allow(ctx context.Context, l *ratelimit.Limiter) error {
	if !l.Allow(ratelimit.AddrKey(clientFrom(ctx).ip), time.Now()) {
		return errTooManyTries
	}
	return nil
}

func (s *server) GetStorefrontEvent(ctx context.Context, req GetStorefrontEventRequestObject) (GetStorefrontEventResponseObject, error) {
	code := ""
	if req.Params.Code != nil {
		code = *req.Params.Code
	}
	// Each read with a code is a guess at one: count them, so codes can't be
	// tried at speed.
	if code != "" {
		if err := allow(ctx, s.codeLimit); err != nil {
			return nil, err
		}
	}
	ev, err := s.Sales.StorefrontEvent(ctx, req.Organizer, req.EventSlug, code)
	if err != nil {
		return nil, salesError(err)
	}
	out := StorefrontEvent{TicketTypes: []StorefrontTicketType{}, Questions: []StorefrontQuestion{}, Promo: nullable.NewNullNullable[StorefrontPromo]()}
	out.Organizer.Name, out.Organizer.Slug = ev.Organizer.Name, ev.Organizer.Slug
	e := ev.Event
	out.Event.Id, out.Event.Slug, out.Event.Name, out.Event.Description = e.ID, e.Slug, e.Name, e.Description
	out.Event.VenueName, out.Event.VenueAddress, out.Event.Timezone = e.VenueName, e.VenueAddress, e.Timezone
	out.Event.StartsAt, out.Event.EndsAt = e.StartsAt, e.EndsAt
	out.Event.FeeMode, out.Event.State = StorefrontEventEventFeeMode(e.FeeMode), StorefrontEventEventState(e.State)
	for _, t := range ev.TicketTypes {
		out.TicketTypes = append(out.TicketTypes, StorefrontTicketType{Id: t.ID, Name: t.Name, Description: t.Description, PriceCents: t.PriceCents,
			MinPerOrder: t.MinPerOrder, MaxPerOrder: t.MaxPerOrder, Remaining: t.Remaining, OnSale: t.OnSale,
			SalesStart: nullOf(t.SalesStart), SalesEnd: nullOf(t.SalesEnd), Unlocked: t.Unlocked})
	}
	if p := ev.Promo; p != nil {
		out.Promo.Set(StorefrontPromo{Code: p.Code, Kind: StorefrontPromoKind(p.Kind), Value: p.Value})
	}
	for _, q := range ev.Questions {
		out.Questions = append(out.Questions, StorefrontQuestion{Id: q.ID, Label: q.Label, Kind: StorefrontQuestionKind(q.Kind),
			Options: q.Options, Required: q.Required, Scope: StorefrontQuestionScope(q.Scope), AppliesToAll: q.AppliesToAll, AppliesTo: q.AppliesTo})
	}
	return GetStorefrontEvent200JSONResponse(out), nil
}

func toCheckout(c sales.Checkout) Checkout {
	out := Checkout{Id: c.ID, EventId: c.EventID, Status: CheckoutStatus(c.Status), ExpiresAt: c.ExpiresAt, Items: []CheckoutItem{},
		PromoCode: nullable.NewNullNullable[string]()}
	for _, it := range c.Items {
		out.Items = append(out.Items, CheckoutItem{TicketTypeId: it.TicketTypeID, Quantity: it.Quantity})
	}
	if c.PromoCode != "" {
		out.PromoCode.Set(c.PromoCode)
	}
	if c.Token != "" {
		out.Token = &c.Token
	}
	if c.OrderURL != "" {
		out.OrderCode, out.OrderUrl = &c.OrderCode, &c.OrderURL
	}
	q := c.Quote
	out.Quote = Quote{Tickets: []QuoteTicket{}, SubtotalCents: q.SubtotalCents, DiscountCents: q.DiscountCents, TaxCents: q.TaxCents,
		FeeCents: q.FeeCents, TotalCents: q.TotalCents, FeeMode: QuoteFeeMode(q.FeeMode)}
	for _, t := range q.Tickets {
		out.Quote.Tickets = append(out.Quote.Tickets, QuoteTicket{TicketTypeId: t.TicketTypeID, PriceCents: t.PriceCents, DiscountCents: t.DiscountCents,
			TaxCents: t.TaxCents, FeeCents: t.FeeCents, ChargeCents: t.ChargeCents})
	}
	return out
}

// CreateCheckout counts the attempt against the caller's address first, then
// checks Turnstile, then holds the tickets.
func (s *server) CreateCheckout(ctx context.Context, req CreateCheckoutRequestObject) (CreateCheckoutResponseObject, error) {
	if err := allow(ctx, s.checkoutLimit); err != nil {
		return nil, err
	}
	tok, err := turnstileToken(req.Body.TurnstileToken)
	if err != nil {
		return nil, err
	}
	if err := s.Turnstile.Verify(ctx, tok, clientFrom(ctx).ip); err != nil {
		if te := turnstileError(ctx, err); te != nil {
			return nil, te
		}
		return nil, err
	}
	in := sales.CheckoutInput{Organizer: req.Body.Organizer, Event: req.Body.Event}
	if req.Body.PromoCode != nil {
		in.PromoCode = *req.Body.PromoCode
	}
	for _, it := range req.Body.Items {
		in.Items = append(in.Items, sales.Item{TicketTypeID: it.TicketTypeId, Quantity: it.Quantity})
	}
	c, err := s.Sales.CreateCheckout(ctx, in)
	if err != nil {
		return nil, salesError(err)
	}
	return CreateCheckout201JSONResponse(toCheckout(c)), nil
}

func (s *server) GetCheckout(ctx context.Context, req GetCheckoutRequestObject) (GetCheckoutResponseObject, error) {
	c, err := s.Sales.GetCheckout(ctx, req.Token)
	if err != nil {
		return nil, salesError(err)
	}
	return GetCheckout200JSONResponse(toCheckout(c)), nil
}

func (s *server) PayCheckout(ctx context.Context, req PayCheckoutRequestObject) (PayCheckoutResponseObject, error) {
	if err := allow(ctx, s.payLimit); err != nil {
		return nil, err
	}
	d := sales.Details{Email: string(req.Body.Email)}
	if req.Body.Attendees != nil {
		d.Attendees = *req.Body.Attendees
	}
	if req.Body.Answers != nil {
		for _, a := range *req.Body.Answers {
			d.Answers = append(d.Answers, sales.Answer{QuestionID: a.QuestionId, TicketIndex: a.TicketIndex, Value: a.Value})
		}
	}
	ps, err := s.Sales.Pay(ctx, req.Token, d)
	if err != nil {
		return nil, salesError(err)
	}
	out := PayCheckout200JSONResponse{Kind: PaymentStartKind(ps.Kind), AmountCents: ps.AmountCents}
	if ps.Kind == "free" {
		out.OrderCode, out.OrderUrl = &ps.OrderCode, &ps.OrderURL
	} else {
		out.ClientSecret, out.PublishableKey, out.Account = &ps.ClientSecret, &ps.PublishableKey, &ps.Account
	}
	return out, nil
}

// GetOrder counts the read against the caller's address, then shows the
// order to whoever holds its token. privateAnswers sets the no-store and
// no-referrer headers.
func (s *server) GetOrder(ctx context.Context, req GetOrderRequestObject) (GetOrderResponseObject, error) {
	if err := allow(ctx, s.orderLimit); err != nil {
		return nil, err
	}
	o, err := s.Sales.Order(ctx, req.Code, req.Params.Token)
	if err != nil {
		return nil, salesError(err)
	}
	out := Order{Code: o.Code, Status: OrderStatus(o.Status), Email: o.Email, Tickets: []OrderTicket{}}
	e := o.Event
	out.Event.Name, out.Event.StartsAt, out.Event.EndsAt, out.Event.Timezone = e.Name, e.StartsAt, e.EndsAt, e.Timezone
	out.Event.VenueName, out.Event.VenueAddress, out.Event.OrganizerName = e.VenueName, e.VenueAddress, e.Organizer
	out.Totals.SubtotalCents, out.Totals.DiscountCents, out.Totals.TaxCents = o.SubtotalCents, o.DiscountCents, o.TaxCents
	out.Totals.FeeCents, out.Totals.TotalCents = o.FeeCents, o.TotalCents
	for _, t := range o.Tickets {
		out.Tickets = append(out.Tickets, OrderTicket{Id: t.ID, TicketTypeName: t.TicketTypeName, AttendeeName: t.AttendeeName,
			Status: OrderTicketStatus(t.Status), Qr: t.QR})
	}
	return GetOrder200JSONResponse(out), nil
}
