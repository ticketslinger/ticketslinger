// Package sales sells tickets: buyers' checkouts and the holds that keep
// their seats while they pay.
//
// Lock order, shared with the catalogue: an event row is only ever key-share
// locked here (through foreign keys), then the quotas of the requested ticket
// types FOR UPDATE in id order, then everything else is read and written in
// later statements. Ticket types are never row-locked.
package sales

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// Service sells one organizer's tickets at a time, as ts_system.
type Service struct {
	Pool *pgxpool.Pool
	Fee  pricing.Fee
	// Payments takes card payments; nil means paid tickets can't be sold.
	Payments plugin.PaymentProvider
	// Direct: payments go to the server's own Stripe account
	// (TS_PAYMENTS=direct), so organizers connect nothing and no fee is taken.
	Direct        bool
	StorefrontURL string // TS_STOREFRONT_URL: base for order links
	SecretKey     []byte // TS_SECRET_KEY: order links are signed with a key derived from it
	// ticketsEmail builds the ticket email; nil means emails.Tickets. Tests
	// set it to make the step before the send fail.
	ticketsEmail func(emails.TicketsData) (mailer.Message, error)
}

type Item struct {
	TicketTypeID uuid.UUID `json:"ticket_type_id"`
	Quantity     int       `json:"quantity"`
}

type CheckoutInput struct {
	Organizer, Event string // slugs, as in the storefront URL
	Items            []Item
	PromoCode        string // "" for none
}

type Checkout struct {
	ID, OrganizerID, EventID uuid.UUID
	Token                    string // plaintext; only CreateCheckout returns it
	Status                   string // "open" | "paying" | "completed" | "expired"
	ExpiresAt                time.Time
	Items                    []Item
	PromoCode                string
	Quote                    pricing.Quote
	OrderCode                string         // once completed
	OrderURL                 string         // once completed: the buyer's link to their order
	Event                    *CheckoutEvent // GetCheckout only: what the checkout page shows and asks
}

// CheckoutEvent is the event a checkout is for, as its page needs it: where
// to send the buyer back to, the names of the cart's ticket types, and the
// questions asked of them.
type CheckoutEvent struct {
	Organizer, Slug, Name, Timezone string
	TicketTypes                     []TicketName
	Questions                       []StorefrontQuestion // in position order, asked of the cart's types
}

type TicketName struct {
	ID   uuid.UUID
	Name string
}

type Availability struct {
	TicketTypeID uuid.UUID `json:"ticket_type_id"`
	Remaining    int       `json:"remaining"`
}

const (
	maxTickets = 100
	// attempts bounds how often a checkout starts over because a quota was
	// linked to or unlinked from its ticket types while it waited.
	attempts = 3
)

var errQuotasMoved = errors.New("sales: the quotas of these ticket types changed")

// fee is the platform fee per ticket: none in direct mode, where the
// server's own Stripe account takes the whole payment.
func (s *Service) fee() pricing.Fee {
	if s.Direct {
		return pricing.Fee{}
	}
	return s.Fee
}

// HashToken is what we store for a buyer's token.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func newToken() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	t := base64.RawURLEncoding.EncodeToString(b)
	return t, HashToken(t)
}

// CreateCheckout holds the tickets in in for 15 minutes, or says why not.
func (s *Service) CreateCheckout(ctx context.Context, in CheckoutInput) (Checkout, error) {
	if err := checkItems(in.Items); err != nil {
		return Checkout{}, err
	}
	if len(in.PromoCode) > 64 { // longer than any code, even with spaces around it
		return Checkout{}, errPromoInvalid
	}
	org, event, err := s.resolveLive(ctx, in.Organizer, in.Event)
	if err != nil {
		return Checkout{}, err
	}
	token, hash := newToken()
	for range attempts {
		out, err := s.hold(ctx, org, event, hash, in)
		if errors.Is(err, errQuotasMoved) {
			continue
		}
		if err != nil {
			return Checkout{}, err
		}
		out.Token = token
		return out, nil
	}
	return Checkout{}, ErrBusy
}

func checkItems(items []Item) error {
	if len(items) == 0 {
		return errNoItems
	}
	total, seen := 0, map[uuid.UUID]bool{}
	for _, it := range items {
		switch {
		case it.Quantity < 1:
			return errNoItems
		case it.Quantity > maxTickets:
			return errTooMany
		case seen[it.TicketTypeID]:
			return errListedTwice
		}
		seen[it.TicketTypeID] = true
		total += it.Quantity
	}
	if total > maxTickets {
		return errTooMany
	}
	return nil
}

// resolveLive finds a live event by its storefront address.
func (s *Service) resolveLive(ctx context.Context, orgSlug, eventSlug string) (org, event uuid.UUID, err error) {
	err = system.Public(ctx, s.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select o.id, e.id from organizers o join events e on e.organizer_id = o.id
			where o.slug = $1 and e.slug = $2`, orgSlug, eventSlug).Scan(&org, &event)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = catalogue.ErrNotFound
	}
	return org, event, err
}

func (s *Service) hold(ctx context.Context, org, event uuid.UUID, hash []byte, in CheckoutInput) (Checkout, error) {
	out := Checkout{OrganizerID: org, EventID: event, Status: "open", Items: in.Items}
	err := system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		if err := offSale(ctx, tx, org); err != nil {
			return err
		}
		ids := itemIDs(in.Items)
		// 1. The one lock order: quotas of every requested type, ascending id.
		sizes, links, err := lockQuotas(ctx, tx, event, ids)
		if err != nil {
			return err
		}
		// 2. Everything else is read after the locks, in later statements, so
		//    a guarded catalogue edit is either fully before or fully after.
		ev, err := onSaleEvent(ctx, tx, event)
		if err != nil {
			return err
		}
		p, err := lockPromo(ctx, tx, event, in.PromoCode)
		if err != nil {
			return err
		}
		tts, err := ticketTypes(ctx, tx, event, ids, p)
		if err != nil {
			return err
		}
		if err := sellable(in.Items, tts, links, ev.timezone); err != nil {
			return err
		}
		if !applies(p, in.Items, tts) {
			return errPromoNotHere
		}
		out.Quote = pricing.Compute(lines(tts, in.Items, p), p.pricing(), ev.feeMode, s.fee())
		// Never hold seats nobody can pay for.
		if out.Quote.TotalCents > 0 {
			if _, err := s.paymentsReady(ctx, tx, org); err != nil {
				return err
			}
		}
		// 3. Fit: every quota a requested type belongs to has room.
		if err := fits(ctx, tx, event, sizes, need(in.Items, links), uuid.Nil, p); err != nil {
			return err
		}
		// 4. Write.
		var promoID *uuid.UUID
		if p != nil {
			promoID = &p.id
			out.PromoCode = p.code
		}
		if err := tx.QueryRow(ctx, `insert into checkouts (organizer_id, event_id, token_hash, promo_code_id, expires_at)
			values ($1, $2, $3, $4, statement_timestamp() + interval '15 minutes') returning id, expires_at`,
			org, event, hash, promoID).Scan(&out.ID, &out.ExpiresAt); err != nil {
			return err
		}
		qty := make([]int, len(in.Items))
		for i, it := range in.Items {
			qty[i] = it.Quantity
		}
		_, err = tx.Exec(ctx, `insert into holds (organizer_id, event_id, checkout_id, ticket_type_id, quantity)
			select $1, $2, $3, unnest($4::uuid[]), unnest($5::int[])`, org, event, out.ID, ids, qty)
		return err
	})
	return out, err
}

func itemIDs(items []Item) []uuid.UUID {
	ids := make([]uuid.UUID, len(items))
	for i, it := range items {
		ids[i] = it.TicketTypeID
	}
	return ids
}

// lockQuotas locks the event's quotas over ticketTypes in id order and
// returns their sizes and each type's quotas. A link added or removed while
// we waited for the locks shows up only in a later statement; then the
// locks taken are the wrong set, and the caller starts over
// (errQuotasMoved) rather than lock more out of order.
func lockQuotas(ctx context.Context, tx pgx.Tx, event uuid.UUID, ticketTypes []uuid.UUID) (map[uuid.UUID]int, map[uuid.UUID][]uuid.UUID, error) {
	sizes := map[uuid.UUID]int{}
	var q uuid.UUID
	var size int
	rows, _ := tx.Query(ctx, `select id, size from quotas
		where event_id = $1 and id in (select quota_id from quota_ticket_types where ticket_type_id = any($2))
		order by id for update`, event, ticketTypes)
	if _, err := pgx.ForEachRow(rows, []any{&q, &size}, func() error { sizes[q] = size; return nil }); err != nil {
		return nil, nil, err
	}
	links, seen := map[uuid.UUID][]uuid.UUID{}, map[uuid.UUID]bool{}
	var tt uuid.UUID
	moved := false
	rows, _ = tx.Query(ctx, `select ticket_type_id, quota_id from quota_ticket_types
		where event_id = $1 and ticket_type_id = any($2)`, event, ticketTypes)
	if _, err := pgx.ForEachRow(rows, []any{&tt, &q}, func() error {
		if _, ok := sizes[q]; !ok {
			moved = true
		}
		seen[q] = true
		links[tt] = append(links[tt], q)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if moved || len(seen) != len(sizes) {
		return nil, nil, errQuotasMoved
	}
	return sizes, links, nil
}

// offSale is ErrUnavailable while org is suspended. Every sale transaction
// asks first: they all run as ts_system, which may call
// organizer_suspended (the storefront role may not).
func offSale(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	var suspended bool
	if err := tx.QueryRow(ctx, `select organizer_suspended($1)`, org).Scan(&suspended); err != nil {
		return err
	}
	if suspended {
		return ErrUnavailable
	}
	return nil
}

type eventTerms struct{ feeMode, timezone string }

// onSaleEvent: the event is live (else ErrNotFound) and not over.
func onSaleEvent(ctx context.Context, tx pgx.Tx, event uuid.UUID) (eventTerms, error) {
	var ev eventTerms
	var live, running bool
	err := tx.QueryRow(ctx, `select status = 'live', statement_timestamp() < ends_at, fee_mode, timezone from events where id = $1`, event).
		Scan(&live, &running, &ev.feeMode, &ev.timezone)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || err == nil && !live:
		return ev, catalogue.ErrNotFound
	case err != nil:
		return ev, err
	case !running:
		return ev, errEventOver
	}
	return ev, nil
}

type promo struct {
	id         uuid.UUID
	code, kind string
	value      int64
}

func (p *promo) pricing() *pricing.Promo {
	if p == nil {
		return nil
	}
	return &pricing.Promo{Kind: p.kind, Value: p.value}
}

// lockPromo locks the code's row, so two checkouts can't both take its last
// use, and checks its window and uses. nil for no code.
func lockPromo(ctx context.Context, tx pgx.Tx, event uuid.UUID, code string) (*promo, error) {
	if code = catalogue.NormalizePromoCode(code); code == "" {
		return nil, nil
	}
	var p promo
	var valid bool
	var maxUses *int
	var used int
	err := tx.QueryRow(ctx, `select id, code, kind, value, max_uses, used_count,
			(valid_from is null or valid_from <= statement_timestamp()) and (valid_until is null or statement_timestamp() < valid_until)
		from promo_codes where event_id = $1 and code = $2 for update`, event, code).
		Scan(&p.id, &p.code, &p.kind, &p.value, &maxUses, &used, &valid)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || err == nil && !valid:
		return nil, errPromoInvalid
	case err != nil:
		return nil, err
	case maxUses == nil:
		return &p, nil
	}
	// A new statement: it sees checkouts committed while we waited for the row.
	var open int
	if err := tx.QueryRow(ctx, `select count(*) from checkouts
		where promo_code_id = $1 and status in ('open', 'paying') and expires_at > statement_timestamp()`, p.id).Scan(&open); err != nil {
		return nil, err
	}
	if used+open >= *maxUses {
		return nil, errPromoUsedUp
	}
	return &p, nil
}

type ticketType struct {
	name                 string
	price                int64
	taxBP                int
	inclusive            bool
	start, end           *time.Time
	notOpen, closed      bool
	min, max             int
	hidden               bool
	unlocked, discounted bool
}

// ticketTypes reads the event's ticket types among ids, with tax resolved
// and what the promo (if any) does to each.
func ticketTypes(ctx context.Context, tx pgx.Tx, event uuid.UUID, ids []uuid.UUID, p *promo) (map[uuid.UUID]ticketType, error) {
	var promoID *uuid.UUID
	if p != nil {
		promoID = &p.id
	}
	rows, _ := tx.Query(ctx, `select t.id, t.name, t.price_cents, coalesce(t.tax_rate_bp, e.tax_rate_bp),
			coalesce(t.prices_include_tax, e.prices_include_tax), t.sales_start, t.sales_end,
			coalesce(statement_timestamp() < t.sales_start, false), coalesce(statement_timestamp() >= t.sales_end, false),
			t.min_per_order, t.max_per_order, t.hidden,
			exists (select 1 from promo_unlocks u where u.promo_code_id = $3 and u.ticket_type_id = t.id),
			coalesce((select applies_to_all from promo_codes where id = $3), false)
			  or exists (select 1 from promo_applies_to a where a.promo_code_id = $3 and a.ticket_type_id = t.id)
		from ticket_types t join events e on e.id = t.event_id
		where t.event_id = $1 and t.id = any($2)`, event, ids, promoID)
	out := map[uuid.UUID]ticketType{}
	var id uuid.UUID
	var t ticketType
	_, err := pgx.ForEachRow(rows, []any{&id, &t.name, &t.price, &t.taxBP, &t.inclusive, &t.start, &t.end,
		&t.notOpen, &t.closed, &t.min, &t.max, &t.hidden, &t.unlocked, &t.discounted}, func() error {
		out[id] = t
		t = ticketType{} // fresh pointers for the next row's times
		return nil
	})
	return out, err
}

// sellable checks each item against its ticket type's terms.
func sellable(items []Item, tts map[uuid.UUID]ticketType, links map[uuid.UUID][]uuid.UUID, zone string) error {
	for _, it := range items {
		t, ok := tts[it.TicketTypeID]
		switch {
		// No quota means nothing to count it against: never sell it.
		case !ok || len(links[it.TicketTypeID]) == 0 || t.hidden && !t.unlocked:
			return errUnknownType
		case t.notOpen:
			return errNotOpen(t.name, *t.start, zone)
		case t.closed:
			return errClosed(t.name, *t.end, zone)
		case it.Quantity < t.min || it.Quantity > t.max:
			return errQuantity(t.name, t.min, t.max)
		}
	}
	return nil
}

// applies: a code must take something off, or unlock, at least one item.
// A code that does neither is refused before it uses up anything.
func applies(p *promo, items []Item, tts map[uuid.UUID]ticketType) bool {
	if p == nil {
		return true
	}
	for _, it := range items {
		if t := tts[it.TicketTypeID]; t.unlocked || t.discounted && p.value > 0 {
			return true
		}
	}
	return false
}

// need is how many seats the items take from each quota.
func need(items []Item, links map[uuid.UUID][]uuid.UUID) map[uuid.UUID]int {
	n := map[uuid.UUID]int{}
	for _, it := range items {
		for _, q := range links[it.TicketTypeID] {
			n[q] += it.Quantity
		}
	}
	return n
}

func lines(tts map[uuid.UUID]ticketType, items []Item, p *promo) []pricing.Line {
	out := make([]pricing.Line, len(items))
	for i, it := range items {
		t := tts[it.TicketTypeID]
		out[i] = pricing.Line{TicketTypeID: it.TicketTypeID, Quantity: it.Quantity, PriceCents: t.price,
			TaxRateBP: t.taxBP, PricesIncludeTax: t.inclusive, Discounted: p != nil && t.discounted}
	}
	return out
}

// fits returns SoldOut unless every quota has room for need on top of what
// is sold and held (leaving out the checkout named by except). The caller
// holds the quota locks. quota_used reads expiry against the transaction's
// start, which is never later than the statement clock: a hold that ran out
// while we waited for the locks still counts, so the error is towards
// refusing a seat, never towards overselling one. p is the buyer's code, which decides which hidden
// types SoldOut may mention.
func fits(ctx context.Context, tx pgx.Tx, event uuid.UUID, sizes, need map[uuid.UUID]int, except uuid.UUID, p *promo) error {
	var ex *uuid.UUID
	if except != uuid.Nil {
		ex = &except
	}
	for q, n := range need {
		var used int
		if err := tx.QueryRow(ctx, `select quota_used($1, $2)`, q, ex).Scan(&used); err != nil {
			return err
		}
		if used+n > sizes[q] {
			return soldOut(ctx, tx, event, p)
		}
	}
	return nil
}

// soldOut lists what's left of the types this buyer can see: every visible
// type, and hidden ones only when their code unlocks them.
func soldOut(ctx context.Context, tx pgx.Tx, event uuid.UUID, p *promo) error {
	var promoID *uuid.UUID
	if p != nil {
		promoID = &p.id
	}
	rows, _ := tx.Query(ctx, `select a.ticket_type_id, a.remaining from event_availability($1) a
		join ticket_types t on t.id = a.ticket_type_id
		where not t.hidden or exists (select 1 from promo_unlocks u where u.promo_code_id = $2 and u.ticket_type_id = t.id)
		order by a.ticket_type_id`, event, promoID)
	avail, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Availability])
	if err != nil {
		return err
	}
	return SoldOut{Available: avail}
}

// checkoutOrg is the organizer of the checkout with this token, or
// ErrNotFound.
func (s *Service) checkoutOrg(ctx context.Context, token string) (uuid.UUID, error) {
	var org *uuid.UUID
	if err := s.Pool.QueryRow(ctx, `select checkout_scope($1)`, HashToken(token)).Scan(&org); err != nil {
		return uuid.Nil, err
	}
	if org == nil {
		return uuid.Nil, catalogue.ErrNotFound
	}
	return *org, nil
}

// GetCheckout finds a checkout by its token. Unknown tokens are ErrNotFound,
// a cancelled event's are errEventCancelled, and a suspended organizer's are
// ErrUnavailable unless completed. A hold that ran out reads as
// expired, unless the order already has a payment: then the card may be
// charged, and it stays paying until the payment is settled. One stopped
// unpaid (SettleOrder) reads as cancelled.
// A completed one carries its order's code and link: the token proves it's
// the buyer asking.
func (s *Service) GetCheckout(ctx context.Context, token string) (Checkout, error) {
	org, err := s.checkoutOrg(ctx, token)
	if err != nil {
		return Checkout{}, err
	}
	var out Checkout
	err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		var p promo
		var promoID, orderID *uuid.UUID
		var feeMode, event string
		var version int
		if err := tx.QueryRow(ctx, `select c.id, c.organizer_id, c.event_id,
				-- The hold is over and the payment was stopped: nothing was charged.
				case when exists (select 1 from payments y where y.order_id = o.id and y.status = 'cancelled') then 'cancelled'
					when c.status = 'completed' or c.expires_at > statement_timestamp() then c.status
					-- A payment exists, so a card may already be charged: it stays paying until the webhook settles it.
					when c.status = 'paying' and exists (select 1 from payments y where y.order_id = o.id) then 'paying'
					else 'expired' end,
				c.expires_at, c.promo_code_id, coalesce(p.code, ''), coalesce(p.kind, ''), coalesce(p.value, 0), e.fee_mode,
				o.id, coalesce(o.code, ''), e.status, coalesce(o.link_version, 0)
			from checkouts c join events e on e.id = c.event_id left join promo_codes p on p.id = c.promo_code_id
			left join orders o on o.checkout_id = c.id
			where c.token_hash = $1`, HashToken(token)).Scan(&out.ID, &out.OrganizerID, &out.EventID, &out.Status, &out.ExpiresAt,
			&promoID, &p.code, &p.kind, &p.value, &feeMode, &orderID, &out.OrderCode, &event, &version); errors.Is(err, pgx.ErrNoRows) {
			return catalogue.ErrNotFound // purged since checkout_scope looked
		} else if err != nil {
			return err
		}
		if event == "cancelled" {
			return errEventCancelled
		}
		if out.Status != "completed" {
			// Off sale; a buyer who already paid keeps the road to their order.
			if err := offSale(ctx, tx, org); err != nil {
				return err
			}
			out.OrderCode = "" // pending: not the buyer's yet
		} else if orderID != nil && version == 0 {
			// The checkout's address never expires, so it hands out only the
			// first link. Once the buyer asks for a new one, it goes by email
			// alone: otherwise this address would outlive every rotation.
			out.OrderURL = s.orderURL(out.OrderCode, *orderID, 0)
		}
		var pp *promo
		if promoID != nil {
			p.id = *promoID
			pp = &p
			out.PromoCode = p.code
		}
		var err error
		if out.Items, out.Quote, err = s.cart(ctx, tx, out.ID, out.EventID, pp, feeMode); err != nil {
			return err
		}
		out.Event, err = checkoutEvent(ctx, tx, out.EventID, itemIDs(out.Items))
		return err
	})
	return out, err
}

func checkoutEvent(ctx context.Context, tx pgx.Tx, event uuid.UUID, types []uuid.UUID) (*CheckoutEvent, error) {
	e := &CheckoutEvent{}
	if err := tx.QueryRow(ctx, `select o.slug::text, e.slug, e.name, e.timezone from events e join organizers o on o.id = e.organizer_id
		where e.id = $1`, event).Scan(&e.Organizer, &e.Slug, &e.Name, &e.Timezone); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `select id, name from ticket_types where event_id = $1 and id = any($2) order by position, id`, event, types)
	var err error
	if e.TicketTypes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TicketName]); err != nil {
		return nil, err
	}
	e.Questions, err = questionsFor(ctx, tx, event, types)
	return e, err
}
