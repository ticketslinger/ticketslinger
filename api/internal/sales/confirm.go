package sales

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
)

// confirmOrder turns a pending order into a paid one if its tickets still
// fit. It locks the order's checkout, then the order (the order Pay takes
// them in, so the two never deadlock), then the quotas of its ticket types
// (ascending id), then checks fits with except = this checkout. Fits: cut
// tickets from orders.quote (16 random bytes each), write answers and
// attendee names from orders.details, used_count + 1 on the promo, delete the
// holds, checkout completed, order paid with paid_at and magic_token_hash =
// sha256(orderToken(...)), and a send_email job with the link. An order that
// isn't pending is a no-op. Task 11 adds the branch for "doesn't fit".
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
		status, email, code string
		event               uuid.UUID
		promo               *uuid.UUID
		quote, details      []byte
		eventName, zone     string
		startsAt            time.Time
	}
	if err := tx.QueryRow(ctx, `select o.status, o.email, o.code, o.event_id, o.promo_code_id, o.quote, o.details, e.name, e.timezone, e.starts_at
		from orders o join events e on e.id = o.event_id where o.id = $1 for update of o`, orderID).
		Scan(&o.status, &o.email, &o.code, &o.event, &o.promo, &o.quote, &o.details, &o.eventName, &o.zone, &o.startsAt); err != nil {
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
	need := map[uuid.UUID]int{}
	for _, t := range q.Tickets {
		for _, quota := range links[t.TicketTypeID] {
			need[quota]++
		}
	}
	if err := fits(ctx, tx, o.event, sizes, need, except, nil); err != nil {
		return false, err
	}

	// Cut the tickets, one per line of the quote, in item order.
	n := len(q.Tickets)
	secrets, names, ticketTypes := make([][]byte, n), make([]string, n), make([]uuid.UUID, n)
	price, disc, tax, fee := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i, t := range q.Tickets {
		secrets[i] = make([]byte, 16)
		_, _ = rand.Read(secrets[i]) // never fails
		if i < len(d.Attendees) {
			names[i] = d.Attendees[i]
		}
		ticketTypes[i], price[i], disc[i], tax[i], fee[i] = t.TicketTypeID, t.PriceCents, t.DiscountCents, t.TaxCents, t.FeeCents
	}
	rows, _ := tx.Query(ctx, `insert into tickets (organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, attendee_name, secret)
		select $1, $2, $3, unnest($4::uuid[]), unnest($5::bigint[]), unnest($6::bigint[]), unnest($7::bigint[]), unnest($8::bigint[]), unnest($9::text[]), unnest($10::bytea[])
		returning id, secret`, org, o.event, orderID, ticketTypes, price, disc, tax, fee, names, secrets)
	bySecret := map[string]uuid.UUID{}
	var id uuid.UUID
	var secret []byte
	if _, err := pgx.ForEachRow(rows, []any{&id, &secret}, func() error { bySecret[string(secret)] = id; return nil }); err != nil {
		return false, err
	}
	for _, a := range d.Answers {
		var ticket *uuid.UUID
		if a.TicketIndex >= 0 {
			t := bySecret[string(secrets[a.TicketIndex])]
			ticket = &t
		}
		if _, err := tx.Exec(ctx, `insert into answers (organizer_id, event_id, order_id, ticket_id, question_id, value) values ($1, $2, $3, $4, $5, $6)`,
			org, o.event, orderID, ticket, a.QuestionID, a.Value); err != nil {
			return false, err
		}
	}
	if o.promo != nil {
		if _, err := tx.Exec(ctx, `update promo_codes set used_count = used_count + 1 where id = $1`, *o.promo); err != nil {
			return false, err
		}
	}
	if checkout != nil {
		if _, err := tx.Exec(ctx, `delete from holds where checkout_id = $1`, except); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `update checkouts set status = 'completed' where id = $1`, except); err != nil {
			return false, err
		}
	}
	token := orderToken(s.SecretKey, orderID)
	hash := sha256.Sum256([]byte(token))
	if _, err := tx.Exec(ctx, `update orders set status = 'paid', paid_at = statement_timestamp(), magic_token_hash = $2 where id = $1`, orderID, hash[:]); err != nil {
		return false, err
	}
	link := s.orderURL(o.code, orderID)
	return true, jobs.Enqueue(ctx, tx, "send_email", map[string]string{
		"to":      o.email,
		"subject": "Your tickets for " + o.eventName,
		"text": "You're going to " + o.eventName + " on " + when(o.startsAt, o.zone) +
			". Your tickets are here, and this link is the only key to them, so keep it to yourself: " + link,
	})
}
