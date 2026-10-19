package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/ratelimit"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/tickets"
)

// providerError logs what the payment provider said and answers 503:
// payments_refused when it answered no, payments_unavailable otherwise. The
// provider's errors carry only its status, type, code and request id, never
// keys, client secrets or tokens.
func providerError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "payment provider", "op", op, "err", err)
	if errors.Is(err, plugin.ErrRefused) {
		return errPaymentsRefused
	}
	return errPaymentsUnavailable
}

// accountInUse: a write would make one Stripe account live for two organizers.
func accountInUse(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "organizers_stripe_account_live"
}

func (s *server) paymentsStatus(ctx context.Context, tx pgx.Tx, org organizers.Organizer) (PaymentsStatus, error) {
	st := PaymentsStatus{Provider: PaymentsStatusProviderNone, Mode: PaymentsModeConnect, LinkMethod: nullable.NewNullNullable[LinkMethod]()}
	if s.Payments != nil {
		st.Provider = PaymentsStatusProvider(s.Payments.Name())
		// The test provider always links; Stripe needs the platform's OAuth client id.
		st.CanLinkExisting = st.Provider == PaymentsStatusProviderTest || s.Config.StripeClientID != ""
	}
	if s.direct() {
		// Nothing to connect or link, whatever the organizer had before.
		st.Mode, st.CanLinkExisting = PaymentsModeDirect, false
		return st, nil
	}
	var method *string
	err := tx.QueryRow(ctx, `select stripe_account_id is not null and stripe_disconnected_at is null, stripe_disconnected_at is not null,
		stripe_charges_enabled, stripe_details_submitted, stripe_link_method from organizers where id = $1`, org.ID).
		Scan(&st.Connected, &st.Disconnected, &st.ChargesEnabled, &st.DetailsSubmitted, &method)
	if method != nil {
		st.LinkMethod.Set(LinkMethod(*method))
	}
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

// direct: the server takes every payment through its own Stripe account
// (TS_PAYMENTS=direct), so organizers have nothing to connect.
func (s *server) direct() bool { return s.Config.PaymentsMode == "direct" }

// paymentsWrite is who may change how an organizer takes payments: an owner
// or admin, in person.
var paymentsWrite = Access{Roles: ownerAdmin, Write: true, SessionOnly: true}

// ConnectPayments creates the organizer's Stripe account once and hands back
// an onboarding link. The network calls sit inside the transaction on
// purpose: the organizer row lock is what makes two clicks produce one
// account, and this runs once per organizer. The lock is FOR NO KEY UPDATE,
// so foreign-key checks (FOR KEY SHARE) from other writes to this organizer
// don't wait on Stripe. stripe_account_id is a unique column, so writing it
// takes the stronger lock; that happens last, after both Stripe calls.
//
// An account created here and later disconnected is switched back on, not
// replaced. The create's idempotency key carries stripe_create_attempt:
// Stripe replays a refusal under the same key for 24 hours, so after a
// definite refusal the next try uses a new key. Anything short of a refusal
// (no answer, a 5xx) may have made the account, so the key stays and a
// retry gets that same account back.
func (s *server) ConnectPayments(ctx context.Context, req ConnectPaymentsRequestObject) (ConnectPaymentsResponseObject, error) {
	var link string
	var linkErr, createErr error
	err := s.inOrganizer(ctx, req.Organizer, paymentsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.direct() {
			return errDirectPayments
		}
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		var account, method *string
		var disconnected bool
		var attempt int
		if err := tx.QueryRow(ctx, `select stripe_account_id, stripe_link_method, stripe_disconnected_at is not null, stripe_create_attempt
			from organizers where id = $1 for no key update`, org.ID).Scan(&account, &method, &disconnected, &attempt); err != nil {
			return err
		}
		linked := method != nil && *method == "linked"
		if linked && !disconnected {
			return errAlreadyConnected
		}
		created := account == nil || linked
		if created {
			id, err := s.Payments.CreateAccount(ctx, fmt.Sprintf("acct-%s-%d", org.ID, attempt))
			if errors.Is(err, plugin.ErrRefused) {
				// Commit only this, so the next try gets past the refusal.
				createErr = err
				_, err = tx.Exec(ctx, `update organizers set stripe_create_attempt = stripe_create_attempt + 1 where id = $1`, org.ID)
				return err
			}
			if err != nil {
				return providerError(ctx, "create account", err)
			}
			account = &id
		}
		base := s.Config.DashboardOrigin + "/o/" + org.Slug + "/payments"
		// A failed link still commits a new account, so a retry reuses it.
		link, linkErr = s.Payments.OnboardingLink(ctx, *account, base+"?refresh=1", base+"?return=1")
		if !created && !disconnected {
			return nil
		}
		_, err := tx.Exec(ctx, `update organizers set stripe_account_id = $2, stripe_link_method = 'created', stripe_disconnected_at = null,
			stripe_connected_at = statement_timestamp(), stripe_charges_enabled = stripe_charges_enabled and $3, stripe_details_submitted = stripe_details_submitted and $3 where id = $1`,
			org.ID, *account, !created)
		if accountInUse(err) {
			return errAccountInUse
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "organizer.stripe_connected",
			EntityType: "organizer", EntityID: org.ID, After: map[string]string{"stripe_account_id": *account}})
	})
	if err != nil {
		return nil, err
	}
	if createErr != nil {
		return nil, providerError(ctx, "create account", createErr)
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
		if s.direct() {
			return errDirectPayments
		}
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		// No row lock: only the non-key flags change, and the last write wins.
		// A disconnect landing in between still wins: the update re-checks it.
		var account *string
		if err := tx.QueryRow(ctx, `select stripe_account_id from organizers where id = $1 and stripe_disconnected_at is null`, org.ID).Scan(&account); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if account == nil {
			return errNotConnected
		}
		st, err := s.Payments.Account(ctx, *account)
		if err != nil {
			return providerError(ctx, "account", err)
		}
		if _, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2 and stripe_disconnected_at is null, stripe_details_submitted = $3 where id = $1`,
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

// linkRedirect is the one callback URI registered with Stripe. The organizer
// comes from the state, never from the URL.
func (s *server) linkRedirect() string { return s.Config.DashboardOrigin + "/stripe/oauth/callback" }

// StartStripeLink hands back Stripe's OAuth page for linking an account the
// organizer already has. Only a hash of the state is kept, tied to this
// session and organizer, for ten minutes.
func (s *server) StartStripeLink(ctx context.Context, req StartStripeLinkRequestObject) (StartStripeLinkResponseObject, error) {
	user, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	var url string
	err = s.inOrganizer(ctx, req.Organizer, paymentsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.direct() {
			return errDirectPayments
		}
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		var live bool
		if err := tx.QueryRow(ctx, `select stripe_account_id is not null and stripe_disconnected_at is null from organizers where id = $1 for no key update`,
			org.ID).Scan(&live); err != nil {
			return err
		}
		if live {
			return errAlreadyConnected
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b) // never fails (crypto/rand)
		state := base64.RawURLEncoding.EncodeToString(b)
		u, err := s.Payments.LinkURL(state, s.linkRedirect(), user.Email)
		if errors.Is(err, plugin.ErrLinkUnavailable) {
			return errOAuthNotConfigured
		}
		if err != nil {
			return err
		}
		url = u
		h := sha256.Sum256([]byte(state))
		_, err = tx.Exec(ctx, `insert into stripe_link_states (state_hash, organizer_id, user_id, session_id) values ($1, $2, $3, $4)`,
			h[:], org.ID, user.ID, sess.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return StartStripeLink200JSONResponse{Url: url}, nil
}

// CompleteStripeLink trades Stripe's code for the account and links it to
// the organizer the state names. The state is used up whatever happens after
// it's found: a failed exchange commits only that. Every way of not owning
// the state (unknown, used, expired, another session's, not a member) is the
// same 400 invalid_state.
func (s *server) CompleteStripeLink(ctx context.Context, req CompleteStripeLinkRequestObject) (CompleteStripeLinkResponseObject, error) {
	user, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.Body.State) > 128 {
		return nil, invalidField("too_long", "state", "That state is longer than Stripe ever sends.")
	}
	if len(req.Body.Code) > 255 {
		return nil, invalidField("too_long", "code", "That code is longer than Stripe ever sends.")
	}
	h := sha256.Sum256([]byte(req.Body.State))
	var orgID *uuid.UUID
	if err := s.Pool.QueryRow(ctx, `select stripe_link_scope($1)`, h[:]).Scan(&orgID); err != nil {
		return nil, err
	}
	if orgID == nil {
		return nil, errInvalidState
	}
	var out StripeLinked
	var linkErr error
	err = db.WithOrganizer(ctx, s.Pool, user.ID, *orgID, func(tx pgx.Tx, role string) error {
		if !slices.Contains(ownerAdmin, role) {
			return &apiError{403, "forbidden_role", "Your role here doesn't allow that."}
		}
		if !sess.MFA {
			return &apiError{403, "mfa_required", "Sign in with a passkey or your authenticator app to manage this organizer."}
		}
		tag, err := tx.Exec(ctx, `delete from stripe_link_states where state_hash = $1 and user_id = $2 and session_id = $3 and expires_at > now()`,
			h[:], user.ID, sess.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errInvalidState
		}
		org := organizers.Organizer{ID: *orgID, Role: role}
		var live bool
		if err := tx.QueryRow(ctx, `select slug::text, name, stripe_account_id is not null and stripe_disconnected_at is null
			from organizers where id = $1 for no key update`, org.ID).Scan(&org.Slug, &org.Name, &live); err != nil {
			return err
		}
		if live { // connected another way since the link started
			linkErr = errAlreadyConnected
			return nil
		}
		if s.Payments == nil {
			linkErr = errPaymentsNotConfigured
			return nil
		}
		account, err := s.Payments.CompleteLink(ctx, req.Body.Code)
		if errors.Is(err, plugin.ErrLinkUnavailable) {
			linkErr = errOAuthNotConfigured
			return nil
		}
		if err != nil {
			linkErr = providerError(ctx, "oauth token", err)
			return nil
		}
		// A savepoint, so a refusal still commits the used-up state.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sp.Exec(ctx, `update organizers set stripe_account_id = $2, stripe_link_method = 'linked', stripe_disconnected_at = null,
			stripe_connected_at = statement_timestamp(), stripe_charges_enabled = false, stripe_details_submitted = false where id = $1`, org.ID, account)
		if accountInUse(err) {
			// Not deauthorized: the token exchange hands back the same platform
			// link the other organizer sells through.
			linkErr = errAccountInUse
			return sp.Rollback(ctx)
		}
		if err != nil {
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		// Stripe's flags now; a failure leaves them off for refresh or account.updated to fix.
		if st, err := s.Payments.Account(ctx, account); err != nil {
			slog.ErrorContext(ctx, "payment provider", "op", "account", "err", err)
		} else if _, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2, stripe_details_submitted = $3 where id = $1`,
			org.ID, st.ChargesEnabled, st.DetailsSubmitted); err != nil {
			return err
		}
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "organizer.stripe_linked",
			EntityType: "organizer", EntityID: org.ID, After: map[string]string{"stripe_account_id": account, "via": "oauth"}}); err != nil {
			return err
		}
		out.Organizer = org.Slug
		out.Status, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if errors.Is(err, db.ErrNotMember) {
		return nil, errInvalidState
	}
	if err != nil {
		return nil, err
	}
	if linkErr != nil {
		return nil, linkErr
	}
	return CompleteStripeLink200JSONResponse(out), nil
}

// unlink deauthorizes a linked account. Stripe answers invalid_client both
// when the account is no longer connected to us and when our client id or
// key mode is wrong, so that answer alone proves nothing: a read of the
// account settles it. No access means Stripe already unlinked it (gone);
// a read that works means we still have access, and the refusal stands.
func (s *server) unlink(ctx context.Context, account string) (gone bool, err error) {
	if s.Payments == nil {
		return false, errPaymentsNotConfigured
	}
	err = s.Payments.Unlink(ctx, account)
	var pe *plugin.ProviderError
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, plugin.ErrLinkUnavailable):
		return false, errOAuthNotConfigured
	case !errors.As(err, &pe) || !pe.Refused || pe.Code != "invalid_client":
		return false, providerError(ctx, "deauthorize", err)
	}
	_, probe := s.Payments.Account(ctx, account)
	switch {
	case errors.Is(probe, plugin.ErrNoAccess):
		slog.InfoContext(ctx, "deauthorize: Stripe had already unlinked the account", "account", account, "probe", probe)
		return true, nil
	case probe == nil || errors.Is(probe, plugin.ErrRefused):
		return false, providerError(ctx, "deauthorize", err) // we still have access: the refusal stands
	}
	return false, providerError(ctx, "account", probe)
}

// DisconnectPayments stops selling through the organizer's Stripe account.
// A linked account has our access removed at Stripe first, and a failure
// there changes nothing; an account created here is only marked, since it
// stays the organizer's either way. The id stays, so payments already made
// still find their orders.
func (s *server) DisconnectPayments(ctx context.Context, req DisconnectPaymentsRequestObject) (DisconnectPaymentsResponseObject, error) {
	var out PaymentsStatus
	err := s.inOrganizer(ctx, req.Organizer, paymentsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.direct() {
			return errDirectPayments
		}
		var account, method *string
		var disconnected bool
		if err := tx.QueryRow(ctx, `select stripe_account_id, stripe_link_method, stripe_disconnected_at is not null
			from organizers where id = $1 for no key update`, org.ID).Scan(&account, &method, &disconnected); err != nil {
			return err
		}
		if account == nil || disconnected {
			return errNothingToDisconnect
		}
		var paying bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from checkouts where status = 'paying' and expires_at > now())`).Scan(&paying); err != nil {
			return err
		}
		if paying {
			return errPaymentsInProgress
		}
		after := map[string]string{"stripe_account_id": *account, "via": "dashboard"}
		if *method == "linked" {
			gone, err := s.unlink(ctx, *account)
			if err != nil {
				return err
			}
			if gone {
				after["note"] = "Stripe had already unlinked it"
			}
		}
		if _, err := tx.Exec(ctx, `update organizers set stripe_disconnected_at = statement_timestamp(), stripe_charges_enabled = false where id = $1`, org.ID); err != nil {
			return err
		}
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "organizer.stripe_disconnected",
			EntityType: "organizer", EntityID: org.ID, After: after}); err != nil {
			return err
		}
		var err error
		out, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, err
	}
	return DisconnectPayments200JSONResponse(out), nil
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
	case errors.Is(err, sales.ErrPaymentsRefused):
		return errPaymentsRefused
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
	out.Questions = toQuestions(ev.Questions)
	return GetStorefrontEvent200JSONResponse(out), nil
}

func toQuestions(qs []sales.StorefrontQuestion) []StorefrontQuestion {
	out := []StorefrontQuestion{}
	for _, q := range qs {
		out = append(out, StorefrontQuestion{Id: q.ID, Label: q.Label, Kind: StorefrontQuestionKind(q.Kind),
			Options: q.Options, Required: q.Required, Scope: StorefrontQuestionScope(q.Scope), AppliesToAll: q.AppliesToAll, AppliesTo: q.AppliesTo})
	}
	return out
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
	if e := c.Event; e != nil {
		out.Event = &CheckoutEvent{Organizer: e.Organizer, Slug: e.Slug, Name: e.Name, Timezone: e.Timezone,
			TicketTypes: []TicketName{}, Questions: toQuestions(e.Questions)}
		for _, t := range e.TicketTypes {
			out.Event.TicketTypes = append(out.Event.TicketTypes, TicketName{Id: t.ID, Name: t.Name})
		}
	}
	q := c.Quote
	out.Quote = Quote{Tickets: []QuoteTicket{}, SubtotalCents: q.SubtotalCents, DiscountCents: q.DiscountCents, TaxCents: q.TaxCents,
		FeeCents: q.FeeCents, TotalCents: q.TotalCents, FeeMode: QuoteFeeMode(q.FeeMode), IncludedTaxCents: q.IncludedTaxCents}
	for _, t := range q.Tickets {
		out.Quote.Tickets = append(out.Quote.Tickets, QuoteTicket{TicketTypeId: t.TicketTypeID, PriceCents: t.PriceCents, DiscountCents: t.DiscountCents,
			TaxCents: t.TaxCents, FeeCents: t.FeeCents, ChargeCents: t.ChargeCents, TaxIncluded: t.TaxIncluded})
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

var errTooManyLinkRequests = &apiError{429, "rate_limited", "That's a lot of link requests from one connection. Wait an hour, then try again."}

// RequestOrderLinks answers the same for every address: the job decides
// whether there is anything to send.
func (s *server) RequestOrderLinks(ctx context.Context, req RequestOrderLinksRequestObject) (RequestOrderLinksResponseObject, error) {
	if !s.linkLimit.Allow(ratelimit.AddrKey(clientFrom(ctx).ip), time.Now()) {
		return nil, errTooManyLinkRequests
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
	if err := s.Sales.RequestOrderLinks(ctx, req.Organizer, req.EventSlug, req.Body.Email); err != nil {
		return nil, salesError(err)
	}
	return RequestOrderLinks204Response{}, nil
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
		out.ClientSecret, out.PublishableKey = &ps.ClientSecret, &ps.PublishableKey
		if ps.Account != "" { // "" in direct mode: Stripe.js loads without stripeAccount
			out.Account = &ps.Account
		}
	}
	return out, nil
}

// maxOrderToken caps the order token read from a URL: a real one is far
// shorter, so anything longer is answered as a wrong token without being
// looked up.
const maxOrderToken = 200

// GetOrder counts the read against the caller's address, then shows the
// order to whoever holds its token. privateAnswers sets the no-store and
// no-referrer headers.
func (s *server) GetOrder(ctx context.Context, req GetOrderRequestObject) (GetOrderResponseObject, error) {
	if err := allow(ctx, s.orderLimit); err != nil {
		return nil, err
	}
	if len(req.Params.Token) > maxOrderToken {
		return nil, salesError(catalogue.ErrNotFound) // as a wrong token
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
	out.Event.Status = OrderEventStatus(e.Status)
	out.RefundedCents, out.ReturnedCents = o.RefundedCents, o.ReturnedCents
	for _, t := range o.Tickets {
		qr, qrPNG := nullable.NewNullNullable[string](), nullable.NewNullNullable[string]()
		if t.QR != "" {
			png, err := tickets.PNG(t.QR)
			if err != nil {
				return nil, err
			}
			qr.Set(t.QR)
			qrPNG.Set(base64.StdEncoding.EncodeToString(png))
		}
		out.Tickets = append(out.Tickets, OrderTicket{Id: t.ID, TicketTypeName: t.TicketTypeName, AttendeeName: t.AttendeeName,
			Status: OrderTicketStatus(t.Status), Refunded: t.Refunded, Qr: qr, QrPng: qrPNG})
	}
	return GetOrder200JSONResponse(out), nil
}

// GetOrderPDF is the order's tickets as a PDF, for whoever holds its token,
// counted as an order read. privateAnswers sets no-store and no-referrer.
func (s *server) GetOrderPDF(ctx context.Context, req GetOrderPDFRequestObject) (GetOrderPDFResponseObject, error) {
	if err := allow(ctx, s.orderLimit); err != nil {
		return nil, err
	}
	if len(req.Params.Token) > maxOrderToken {
		return nil, salesError(catalogue.ErrNotFound) // as a wrong token
	}
	pdf, name, err := s.Sales.OrderPDF(ctx, req.Code, req.Params.Token)
	if err != nil {
		return nil, salesError(err)
	}
	return GetOrderPDF200ApplicationpdfResponse{Body: bytes.NewReader(pdf), ContentLength: int64(len(pdf)),
		Headers: GetOrderPDF200ResponseHeaders{ContentDisposition: `attachment; filename="` + name + `"`}}, nil
}

// RequestTicketEmail is the buyer's "Email me my tickets again", counted as
// an order read.
func (s *server) RequestTicketEmail(ctx context.Context, req RequestTicketEmailRequestObject) (RequestTicketEmailResponseObject, error) {
	if err := allow(ctx, s.orderLimit); err != nil {
		return nil, err
	}
	if len(req.Body.Token) > 200 {
		return nil, fieldError{&apiError{422, "bad_token", "That isn't the link from your email. Open your order from the email and try again."}, "token"}
	}
	if err := s.Sales.RequestTicketEmail(ctx, req.Code, req.Body.Token); err != nil {
		return nil, salesError(err)
	}
	return RequestTicketEmail202Response{}, nil
}

// ordersRead keeps check-in staff out of orders: they carry what people paid
// (spec D22). Door staff find guests with searchCheckinTickets instead.
var ordersRead = Access{Roles: ownerAdmin}

// ListOrders shows buyers' emails to owners, admins and keys; nothing here logs them.
func (s *server) ListOrders(ctx context.Context, req ListOrdersRequestObject) (ListOrdersResponseObject, error) {
	out := ListOrders200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, ordersRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		list, err := sales.ListOrders(ctx, tx, req.Event)
		for _, o := range list {
			out = append(out, OrderSummary{Id: o.ID, Code: o.Code, Email: o.Email, Status: OrderSummaryStatus(o.Status), TicketCount: o.TicketCount,
				TotalCents: o.TotalCents, FeeCents: o.FeeCents, FeeMode: OrderSummaryFeeMode(o.FeeMode), CreatedAt: o.CreatedAt, PaidAt: nullOf(o.PaidAt),
				RefundedCents: o.RefundedCents, FeeRefundedCents: o.FeeRefundedCents, TicketEmail: toOrderEmail(o.Mail)})
		}
		return err
	})
	if err != nil {
		return nil, salesError(err)
	}
	return out, nil
}

// GetOrderDetail shows the buyer's email and tickets to owners, admins and keys,
// as ListOrders does; nothing here logs them.
func (s *server) GetOrderDetail(ctx context.Context, req GetOrderDetailRequestObject) (GetOrderDetailResponseObject, error) {
	var d sales.OrderDetailView
	err := s.inOrganizer(ctx, req.Organizer, ordersRead, func(_ organizers.Organizer, tx pgx.Tx) (err error) {
		d, err = sales.OrderDetail(ctx, tx, req.Event, req.Order)
		return err
	})
	if err != nil {
		return nil, salesError(err)
	}
	out := GetOrderDetail200JSONResponse{Id: d.ID, Code: d.Code, Email: d.Email, Status: OrderDetailStatus(d.Status), TicketCount: d.TicketCount,
		TotalCents: d.TotalCents, FeeCents: d.FeeCents, FeeMode: OrderDetailFeeMode(d.FeeMode), CreatedAt: d.CreatedAt, PaidAt: nullOf(d.PaidAt),
		RefundedCents: d.RefundedCents, FeeRefundedCents: d.FeeRefundedCents, TicketEmail: toOrderEmail(d.Mail), Tickets: []OrderDetailTicket{}, Refunds: []Refund{}}
	for _, t := range d.Tickets {
		out.Tickets = append(out.Tickets, OrderDetailTicket{Id: t.ID, TicketTypeName: t.TicketTypeName, AttendeeName: t.AttendeeName,
			Status: OrderDetailTicketStatus(t.Status), ChargeCents: t.ChargeCents, FeeCents: t.FeeCents, RefundId: nullOf(t.RefundID)})
	}
	for _, r := range d.Refunds {
		out.Refunds = append(out.Refunds, toRefund(r))
	}
	return out, nil
}

func toRefund(r sales.RefundView) Refund {
	out := Refund{Id: r.ID, AmountCents: r.AmountCents, FeeCents: r.FeeCents, FeeBack: r.FeeBack, Status: RefundStatus(r.Status), ReturnedByBank: r.ReturnedByBank,
		Reason: RefundReason(r.Reason), CreatedAt: r.CreatedAt, Failure: nullable.NewNullNullable[RefundFailure]()}
	if r.Failure != nil {
		out.Failure.Set(RefundFailure(*r.Failure))
	}
	return out
}

func toOrderEmail(m sales.MailState) OrderEmail {
	out := OrderEmail{Attempts: m.Attempts, Status: nullable.NewNullNullable[OrderEmailStatus](),
		Problem: nullable.NewNullNullable[OrderEmailProblem](), UpdatedAt: nullOf(m.UpdatedAt)}
	if m.Status != nil {
		out.Status.Set(OrderEmailStatus(*m.Status))
	}
	if m.Problem != nil {
		out.Problem.Set(OrderEmailProblem(*m.Problem))
	}
	return out
}

// ResendTicketEmail queues the order's ticket email again. It moves no
// money, so a write key may ask too.
func (s *server) ResendTicketEmail(ctx context.Context, req ResendTicketEmailRequestObject) (ResendTicketEmailResponseObject, error) {
	var out ResendTicketEmail202JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, catalogueWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		m, err := sales.ResendTicketEmail(ctx, tx, org.ID, req.Event, req.Order, actorFrom(ctx))
		out = ResendTicketEmail202JSONResponse(toOrderEmail(m))
		return err
	})
	if err != nil {
		return nil, salesError(err)
	}
	return out, nil
}

// refundsWrite: money leaving the organizer's Stripe balance is a person's
// call, never an integration's (a leaked key mustn't refund an event).
var refundsWrite = Access{Roles: ownerAdmin, Write: true, SessionOnly: true}

// RefundOrder refunds chosen tickets of an order, or all it has left. Stripe
// is asked by the refund job after this commits, never inside it.
func (s *server) RefundOrder(ctx context.Context, req RefundOrderRequestObject) (RefundOrderResponseObject, error) {
	var out RefundOrder201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, refundsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		if req.Body.RefundFee == nil {
			return missingField("refund_fee", "true to give our fee back too, false to keep it")
		}
		in := sales.RefundRequest{FeeBack: *req.Body.RefundFee}
		// Absent: the whole order. Present, a list of at least one; null or
		// [] names no tickets (bad_tickets), never "all of them".
		if req.Body.TicketIds.IsSpecified() {
			ids, _ := req.Body.TicketIds.Get() // null: an error, and no ids
			in.TicketIDs = append([]uuid.UUID{}, ids...)
		}
		res, err := sales.RefundTickets(ctx, tx, org.ID, req.Event, req.Order, in, "organizer", actorFrom(ctx))
		if err != nil {
			return err
		}
		out = RefundOrder201JSONResponse{TicketIds: res.TicketIDs, AmountCents: res.AmountCents, FeeCents: res.FeeCents,
			OrderStatus: RefundResultOrderStatus(res.OrderStatus), Refund: nullable.NewNullNullable[Refund]()}
		if res.RefundID != nil {
			r, err := sales.RefundByID(ctx, tx, *res.RefundID)
			if err != nil {
				return err
			}
			out.Refund.Set(toRefund(r))
		}
		return nil
	})
	if err != nil {
		return nil, salesError(err)
	}
	return out, nil
}

// RetryRefund sends a failed refund again, as the same refund.
func (s *server) RetryRefund(ctx context.Context, req RetryRefundRequestObject) (RetryRefundResponseObject, error) {
	var out RetryRefund200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, refundsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		r, err := sales.RetryRefund(ctx, tx, org.ID, req.Event, req.Order, req.Refund, actorFrom(ctx))
		if err != nil {
			return err
		}
		out = RetryRefund200JSONResponse(toRefund(r))
		return nil
	})
	if err != nil {
		return nil, salesError(err)
	}
	return out, nil
}

// CancelEvent takes a live event off sale and refunds every paid order, our
// fee included. Stripe is asked by the jobs this queues, after it commits.
func (s *server) CancelEvent(ctx context.Context, req CancelEventRequestObject) (CancelEventResponseObject, error) {
	var out CancelEvent200JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, refundsWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		res, err := sales.CancelEvent(ctx, tx, org.ID, req.Event, req.Body.ConfirmName, actorFrom(ctx))
		if err != nil {
			return err
		}
		e, err := catalogue.GetEvent(ctx, tx, req.Event)
		if err != nil {
			return err
		}
		out = CancelEvent200JSONResponse{Event: toEvent(e), Orders: res.Orders, AmountCents: res.AmountCents, FeeCents: res.FeeCents}
		return nil
	})
	if err != nil {
		return nil, salesError(err)
	}
	return out, nil
}
