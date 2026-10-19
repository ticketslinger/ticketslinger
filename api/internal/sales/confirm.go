package sales

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// confirmOrder turns a pending order into a paid one if its tickets can
// still be sold. It locks the order's checkout, then the order (the order Pay
// takes them in, so the two never deadlock), then the quotas of its ticket
// types (ascending id), then the promo code; everything it decides on is
// read after the quota locks. Sellable: cut tickets from orders.quote (16 random bytes each), write
// answers and attendee names from orders.details, used_count + 1 on the
// promo, delete the holds, checkout completed, order paid with paid_at and
// magic_token_hash = sha256(orderToken(...)), and an order_confirmation job
// that names the order (never the link: see OrderConfirmation). An order that
// isn't pending is a no-op.
//
// Not sellable any more (unsellable says why): a free order returns the
// buyer's error, so Pay rolls back. A paid one is cancelled and refunded
// (cancelLate) and confirmOrder returns paid = false. There's one check for
// an on-time and a late payment: with the hold still live it fits.
func (s *Service) confirmOrder(ctx context.Context, tx pgx.Tx, org, orderID uuid.UUID) (paid bool, err error) {
	var checkout *uuid.UUID
	if err := tx.QueryRow(ctx, `select checkout_id from orders where id = $1`, orderID).Scan(&checkout); err != nil {
		return false, err
	}
	except := uuid.Nil
	if checkout != nil {
		except = *checkout
		if _, err := tx.Exec(ctx, `select 1 from checkouts where id = $1 for update`, except); err != nil {
			return false, err
		}
	}
	var o struct {
		status         string
		event          uuid.UUID
		promo          *uuid.UUID
		total          int64
		quote, details []byte
	}
	if err := tx.QueryRow(ctx, `select status, event_id, promo_code_id, total_cents, quote, details from orders where id = $1 for update`, orderID).
		Scan(&o.status, &o.event, &o.promo, &o.total, &o.quote, &o.details); err != nil {
		return false, err
	}
	if o.status != "pending" {
		return false, nil
	}
	var q pricing.Quote
	var d Details
	if err := json.Unmarshal(o.quote, &q); err != nil {
		return false, err
	}
	if err := json.Unmarshal(o.details, &d); err != nil {
		return false, err
	}
	var types []uuid.UUID
	for _, t := range q.Tickets {
		if !slices.Contains(types, t.TicketTypeID) {
			types = append(types, t.TicketTypeID)
		}
	}
	sizes, links, err := lockQuotas(ctx, tx, o.event, types)
	if errors.Is(err, errQuotasMoved) {
		return false, ErrBusy
	}
	if err != nil {
		return false, err
	}
	no, err := stillSellable(ctx, tx, o.event, o.promo, q, d, links, sizes, except)
	if err != nil {
		return false, err
	}
	if no != nil {
		if o.total == 0 {
			return false, no.err
		}
		return false, s.cancelLate(ctx, tx, org, orderID, checkout, no.reason)
	}
	if err := cut(ctx, tx, org, o.event, orderID, q, d); err != nil {
		return false, err
	}
	if o.promo != nil {
		if _, err := tx.Exec(ctx, `update promo_codes set used_count = used_count + 1 where id = $1`, *o.promo); err != nil {
			return false, err
		}
	}
	if err := closeCheckout(ctx, tx, checkout); err != nil {
		return false, err
	}
	if err := s.settle(ctx, tx, orderID, "paid"); err != nil {
		return false, err
	}
	return true, jobs.Enqueue(ctx, tx, OrderConfirmationJob, confirmation{Order: orderID, Organizer: org})
}

// unsellable is why a pending order can't become tickets: reason for the
// audit log, err for a buyer who is still at the checkout (a free order).
type unsellable struct {
	reason string
	err    error
}

// stillSellable checks, under the quota locks, that the order's tickets can
// still be sold: the event is live and not over, every ticket type is still
// there and counted by a quota, the quotas have room (leaving out this
// order's own checkout), every question answered is still there, and the
// promo code has a use left. The promo row is locked after the quotas, as
// CreateCheckout locks it. nil means it can.
//
// The questions aren't locked: a DeleteQuestion racing between this check
// and the answers' insert makes that insert fail its foreign key (23503).
// That rolls back the whole event, stripe_events row included, so the
// webhook answers 5xx, Stripe sends it again, and this check then finds the
// question gone.
func stillSellable(ctx context.Context, tx pgx.Tx, event uuid.UUID, promo *uuid.UUID, q pricing.Quote, d Details,
	links map[uuid.UUID][]uuid.UUID, sizes map[uuid.UUID]int, except uuid.UUID) (*unsellable, error) {
	var live, running bool
	if err := tx.QueryRow(ctx, `select status = 'live', statement_timestamp() < ends_at from events where id = $1`, event).Scan(&live, &running); err != nil {
		return nil, err
	}
	if !live {
		return &unsellable{"event_not_live", catalogue.ErrNotFound}, nil
	}
	if !running {
		return &unsellable{"event_over", errEventOver}, nil
	}
	need := map[uuid.UUID]int{}
	for _, t := range q.Tickets {
		// A deleted type takes its quota links with it.
		if len(links[t.TicketTypeID]) == 0 {
			return &unsellable{"ticket_type_gone", errUnknownType}, nil
		}
		for _, quota := range links[t.TicketTypeID] {
			need[quota]++
		}
	}
	var so SoldOut
	if err := fits(ctx, tx, event, sizes, need, except, nil); errors.As(err, &so) {
		return &unsellable{"sold_out", err}, nil
	} else if err != nil {
		return nil, err
	}
	var asked []uuid.UUID
	for _, a := range d.Answers {
		if !slices.Contains(asked, a.QuestionID) {
			asked = append(asked, a.QuestionID)
		}
	}
	if len(asked) > 0 {
		var left int
		if err := tx.QueryRow(ctx, `select count(*) from questions where event_id = $1 and id = any($2)`,
			event, asked).Scan(&left); err != nil {
			return nil, err
		}
		if left != len(asked) {
			return &unsellable{"question_gone", errUnknownType}, nil
		}
	}
	if promo != nil {
		var maxUses *int
		var used int
		err := tx.QueryRow(ctx, `select max_uses, used_count from promo_codes where id = $1 for update`, *promo).Scan(&maxUses, &used)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) { // deleted since: the order keeps its price
			return nil, err
		}
		if err == nil && maxUses != nil && used+1 > *maxUses {
			return &unsellable{"promo_used_up", errPromoUsedUp}, nil
		}
	}
	return nil, nil
}

// cut writes the order's tickets, one per line of the quote in item order,
// and its answers.
func cut(ctx context.Context, tx pgx.Tx, org, event, orderID uuid.UUID, q pricing.Quote, d Details) error {
	n := len(q.Tickets)
	secrets, names, ticketTypes := make([][]byte, n), make([]string, n), make([]uuid.UUID, n)
	price, disc, tax, fee, charge := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i, t := range q.Tickets {
		secrets[i] = make([]byte, 16)
		_, _ = rand.Read(secrets[i]) // never fails
		if i < len(d.Attendees) {
			names[i] = d.Attendees[i]
		}
		ticketTypes[i], price[i], disc[i], tax[i], fee[i] = t.TicketTypeID, t.PriceCents, t.DiscountCents, t.TaxCents, t.FeeCents
		charge[i] = t.ChargeCents
	}
	rows, _ := tx.Query(ctx, `insert into tickets (organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, attendee_name, secret)
		select $1, $2, $3, unnest($4::uuid[]), unnest($5::bigint[]), unnest($6::bigint[]), unnest($7::bigint[]), unnest($8::bigint[]), unnest($9::bigint[]), unnest($10::text[]), unnest($11::bytea[])
		returning id, secret`, org, event, orderID, ticketTypes, price, disc, tax, fee, charge, names, secrets)
	bySecret := map[string]uuid.UUID{}
	var id uuid.UUID
	var secret []byte
	if _, err := pgx.ForEachRow(rows, []any{&id, &secret}, func() error { bySecret[string(secret)] = id; return nil }); err != nil {
		return err
	}
	for _, a := range d.Answers {
		var ticket *uuid.UUID
		if a.TicketIndex >= 0 {
			t := bySecret[string(secrets[a.TicketIndex])]
			ticket = &t
		}
		if _, err := tx.Exec(ctx, `insert into answers (organizer_id, event_id, order_id, ticket_id, question_id, value) values ($1, $2, $3, $4, $5, $6)`,
			org, event, orderID, ticket, a.QuestionID, a.Value); err != nil {
			return err
		}
	}
	return nil
}

// closeCheckout gives the checkout's seats up (they are tickets now, or
// nobody's) and marks it completed, so it can't be paid again.
func closeCheckout(ctx context.Context, tx pgx.Tx, checkout *uuid.UUID) error {
	if checkout == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `delete from holds where checkout_id = $1`, *checkout); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `update checkouts set status = 'completed' where id = $1`, *checkout)
	return err
}

// settle gives the order its final status and its link: a cancelled order's
// page tells the buyer what happened. The payment's client secret goes: the
// intent is paid or being refunded, so the secret has no use left.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, status string) error {
	hash := sha256.Sum256([]byte(orderToken(s.SecretKey, orderID, 0))) // a new order: its first link
	if _, err := tx.Exec(ctx, `update orders set status = $2, magic_token_hash = $3,
		paid_at = case when $2 = 'paid' then statement_timestamp() end where id = $1`, orderID, status, hash[:]); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `update payments set client_secret = '' where order_id = $1`, orderID)
	return err
}

// cancelLate is the paid order that can't be sold any more: the payment went
// through, but the seats (or the event, a ticket type, the code's last use)
// went first. Cancel it, free the checkout's holds, record it as the
// system's own decision, and request a refund of all of it, our fee included.
func (s *Service) cancelLate(ctx context.Context, tx pgx.Tx, org, orderID uuid.UUID, checkout *uuid.UUID, reason string) error {
	slog.WarnContext(ctx, "paid order can't be sold any more: cancelling and refunding", "order", orderID, "reason", reason)
	if err := closeCheckout(ctx, tx, checkout); err != nil {
		return err
	}
	if err := s.settle(ctx, tx, orderID, "cancelled"); err != nil {
		return err
	}
	if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{System: true}, Action: "order.cancelled_late",
		EntityType: "order", EntityID: orderID, After: map[string]string{"reason": reason}}); err != nil {
		return err
	}
	var payment uuid.UUID
	var amount, fee int64
	if err := tx.QueryRow(ctx, `select id, amount_cents, application_fee_cents from payments where order_id = $1`, orderID).Scan(&payment, &amount, &fee); err != nil {
		return err
	}
	_, err := requestRefund(ctx, tx, org, payment, amount, fee, true, "late_payment", nil)
	return err
}

// OrderConfirmationJob emails a paid order's link to its buyer.
const OrderConfirmationJob = "order_confirmation"

// confirmation is the job's whole payload. The link is the only key to the
// tickets, so it is rebuilt at send time and never stored: not in jobs (no
// RLS, readable by the app role, in every backup), not anywhere.
type confirmation struct {
	Order     uuid.UUID `json:"order_id"`
	Organizer uuid.UUID `json:"organizer_id"`
}

// OrderConfirmation handles OrderConfirmationJob. It reads the order and
// its valid tickets as ts_system for the named organizer and sends outside
// the transaction. An order that is gone or not paid sends nothing, and so
// does one with no valid ticket left: there's nothing to bring to the door,
// and the refund email already told the buyer why. Delivery is at least
// once, as for every job.
func (s *Service) OrderConfirmation(m mailer.Mailer) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var c confirmation
		if err := json.Unmarshal(payload, &c); err != nil {
			return err
		}
		d := emails.TicketsData{OrderID: c.Order, OrganizerID: c.Organizer}
		var zone, orgSlug, eventSlug string
		var starts time.Time
		var version int
		err := system.Run(ctx, s.Pool, c.Organizer, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select o.email::text, o.code, e.name, e.timezone, e.starts_at, e.venue_name, g.name, g.slug, e.slug, o.link_version
				from orders o join events e on e.id = o.event_id join organizers g on g.id = o.organizer_id
				where o.id = $1 and o.status in ('paid', 'partially_refunded')`, c.Order).
				Scan(&d.To, &d.Code, &d.Event, &zone, &starts, &d.Venue, &d.Organizer, &orgSlug, &eventSlug, &version); err != nil {
				return err
			}
			rows, _ := tx.Query(ctx, `select tt.name, t.attendee_name, t.secret from tickets t join ticket_types tt on tt.id = t.ticket_type_id
				where t.order_id = $1 and t.status = 'valid' order by t.id`, c.Order)
			var t emails.Ticket
			var secret []byte
			_, err := pgx.ForEachRow(rows, []any{&t.Type, &t.Attendee, &secret}, func() error {
				t.QR = base64.RawURLEncoding.EncodeToString(secret)
				d.Tickets = append(d.Tickets, t)
				return nil
			})
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && len(d.Tickets) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		base := strings.TrimRight(s.StorefrontURL, "/")
		d.When = when(starts, zone)
		d.OrderURL = s.orderURL(d.Code, c.Order, version)
		d.LostLinkURL = base + "/" + orgSlug + "/" + eventSlug + "/order-link"
		msg, err := emails.Tickets(d)
		if err != nil {
			return err
		}
		return jobs.Deliver(ctx, m, msg)
	}
}
