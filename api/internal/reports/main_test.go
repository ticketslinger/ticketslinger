package reports_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/reports"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// fixture is organizer acme, its owner, and its event gala in Denver selling
// General and VIP from one quota of 10, with a promo code and two questions.
type fixture struct {
	app, owner          *pgxpool.Pool
	user, org, event    uuid.UUID
	general, vip, promo uuid.UUID
	dietary, tshirt     uuid.UUID
}

func seed(t *testing.T) fixture {
	t.Helper()
	f := fixture{app: testdb.New(t)}
	f.owner = testdb.Owner(t, f.app)
	f.user = f.id(t, `insert into users(email, name) values ('owner@example.com', 'Owner') returning id`)
	f.org = f.id(t, `insert into organizers(name, slug) values ('Acme', 'acme') returning id`)
	f.exec(t, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'owner')`, f.org, f.user)
	f.event = f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'gala', 'Gala', 'America/Denver', '2026-03-05T02:00:00Z', '2026-03-05T05:00:00Z', 'live') returning id`, f.org)
	// VIP goes in first so position, not insertion, has to order them.
	f.vip = f.id(t, `insert into ticket_types(organizer_id, event_id, name, price_cents, position) values ($1, $2, 'VIP', 5000, 2) returning id`, f.org, f.event)
	f.general = f.id(t, `insert into ticket_types(organizer_id, event_id, name, price_cents, position) values ($1, $2, 'General', 2500, 1) returning id`, f.org, f.event)
	q := f.id(t, `insert into quotas(organizer_id, event_id, name, size) values ($1, $2, 'Hall', 10) returning id`, f.org, f.event)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4), ($1, $2, $3, $5)`,
		f.org, f.event, q, f.general, f.vip)
	f.promo = f.id(t, `insert into promo_codes(organizer_id, event_id, code, kind, value) values ($1, $2, 'EARLY', 'percent', 1000) returning id`, f.org, f.event)
	f.dietary = f.id(t, `insert into questions(organizer_id, event_id, label, kind, scope, position) values ($1, $2, 'Dietary needs', 'text', 'order', 1) returning id`, f.org, f.event)
	f.tshirt = f.id(t, `insert into questions(organizer_id, event_id, label, kind, scope, position) values ($1, $2, 'T-shirt', 'text', 'ticket', 2) returning id`, f.org, f.event)
	return f
}

// sales adds A and B on 28 February and C and L on 1 March, Denver evenings
// that cross UTC midnight:
//   - A: two General, promo EARLY, one checked in.
//   - B: one VIP, partly refunded: 2,500 back with 50 of our fee.
//   - C: one General, fee absorbed; a failed refund of 100, and a requested
//     refund of 1,000 whose 30 of our fee hasn't gone back yet.
//   - L: a late payment: cancelled before it paid, so no paid_at and no
//     tickets, its payment succeeded anyway and went back in full.
//   - X: abandoned, its payment failed. The order list leaves it out.
//
// Our fee on each payment is the order's fee_cents. It returns A and its tickets.
func (f fixture) sales(t *testing.T) (orderA uuid.UUID, ticketsA []uuid.UUID) {
	t.Helper()
	orderA, ticketsA = f.order(t, sale{code: "TS-AAAAA", email: "a@example.com", placed: "2026-03-01T01:28:00Z", paid: true,
		subtotal: 5000, discount: 500, fee: 100, total: 4600, promo: &f.promo, ticketType: f.general, names: []string{"Ada", "Bea"}})
	f.exec(t, `update tickets set checked_in_at = '2026-03-05T02:10:00Z' where id = $1`, ticketsA[0])
	// Scanned in by the owner at North, then again by a scanner app's key.
	key := f.id(t, `insert into api_keys(organizer_id, name, key_hash, prefix, scope) values ($1, 'Scanner', '\x01', 'abcd1234', 'write') returning id`, f.org)
	f.exec(t, `insert into checkins(organizer_id, event_id, ticket_id, value_hash, method, gate, result, actor_user_id, actor_api_key_id, at)
		values ($1, $2, $3, sha256('x'), 'scan', 'North', 'accepted', $4, null, '2026-03-05T02:10:00Z'),
		       ($1, $2, $3, sha256('x'), 'scan', null, 'duplicate', null, $5, '2026-03-05T02:11:00Z')`, f.org, f.event, ticketsA[0], f.user, key)
	f.exec(t, `update promo_codes set used_count = 1 where id = $1`, f.promo)
	f.answer(t, orderA, nil, f.dietary, "Vegan")
	f.answer(t, orderA, &ticketsA[0], f.tshirt, "M")
	orderB, _ := f.order(t, sale{code: "TS-BBBBB", email: "b@example.com", placed: "2026-03-01T04:58:00Z", paid: true, status: "partially_refunded",
		subtotal: 5000, tax: 400, fee: 100, total: 5500, ticketType: f.vip, names: []string{"Cy"}})
	f.refund(t, orderB, 2500, 50, "succeeded")
	orderC, _ := f.order(t, sale{code: "TS-CCCCC", email: "c@example.com", placed: "2026-03-02T02:58:00Z", paid: true, absorb: true,
		subtotal: 2500, fee: 100, total: 2500, ticketType: f.general, names: []string{"Dee"}})
	f.refund(t, orderC, 100, 0, "failed")
	f.refund(t, orderC, 1000, 30, "requested")
	orderL, _ := f.order(t, sale{code: "TS-PPPPP", email: "l@example.com", placed: "2026-03-02T04:00:00Z", status: "cancelled",
		subtotal: 2500, fee: 100, total: 2600})
	f.refund(t, orderL, 2600, 100, "succeeded")
	f.order(t, sale{code: "TS-XXXXX", email: "x@example.com", placed: "2026-03-02T05:00:00Z", status: "cancelled", payment: "failed",
		subtotal: 7000, fee: 100, total: 7100})
	return orderA, ticketsA
}

// sale is one order to seed. Its payment (succeeded unless payment says
// otherwise) carries the order's fee_cents as our fee; a paid one was paid
// two minutes after it was placed.
type sale struct {
	code, email, placed, status, payment string
	paid, absorb                         bool
	subtotal, discount, tax, fee, total  int64
	promo                                *uuid.UUID
	ticketType                           uuid.UUID
	names                                []string
}

func (f fixture) order(t *testing.T, s sale) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	status, mode, payment := cmp.Or(s.status, "paid"), "pass", cmp.Or(s.payment, "succeeded")
	if s.absorb {
		mode = "absorb"
	}
	o := f.id(t, `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents,
			fee_mode, promo_code_id, quote, details, created_at, paid_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, '{}', '{}', $13, case when $14 then $13::timestamptz + interval '2 minutes' end) returning id`,
		f.org, f.event, s.code, s.email, status, s.subtotal, s.discount, s.tax, s.fee, s.total, mode, s.promo, s.placed, s.paid)
	f.exec(t, `insert into payments(organizer_id, order_id, provider, stripe_account_id, payment_intent_id, client_secret, amount_cents, application_fee_cents, status)
		values ($1, $2, 'test', 'acct_test', 'pi_' || $3, 'secret_' || $3, $4, $5, $6)`, f.org, o, s.code, max(s.total, 50), s.fee, payment)
	ids := make([]uuid.UUID, len(s.names))
	for i, name := range s.names {
		secret := make([]byte, 16)
		_, _ = rand.Read(secret)
		ids[i] = f.id(t, `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, attendee_name, secret)
			values ($1, $2, $3, $4, 0, 0, 0, 0, 0, $5, $6) returning id`, f.org, f.event, o, s.ticketType, name, secret)
	}
	return o, ids
}

// refund adds a refund of order's payment. Our fee on a succeeded one has
// gone back; on a requested one it hasn't yet.
func (f fixture) refund(t *testing.T, order uuid.UUID, amount, fee int64, status string) {
	t.Helper()
	f.exec(t, `insert into refunds(organizer_id, payment_id, amount_cents, fee_refunded, status, reason, fee_cents, fee_refunded_at, failure)
		select organizer_id, id, $2, $3 > 0, $4, 'organizer', $3,
			case when $4 = 'succeeded' and $3 > 0 then now() end, case when $4 = 'failed' then 'gave_up' end
		from payments where order_id = $1`, order, amount, fee, status)
}

func (f fixture) answer(t *testing.T, order uuid.UUID, ticket *uuid.UUID, question uuid.UUID, value string) {
	t.Helper()
	f.exec(t, `insert into answers(organizer_id, event_id, order_id, ticket_id, question_id, value) values ($1, $2, $3, $4, $5, $6)`,
		f.org, f.event, order, ticket, question, value)
}

func (f fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) id(t *testing.T, sql string, args ...any) (id uuid.UUID) {
	t.Helper()
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// report reads the event's report as its owner, under row-level security.
func (f fixture) report(t *testing.T) (r reports.Report) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		r, err = reports.Event(ctx, tx, f.event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// csv writes one export as the owner and returns its filename and bytes.
func (f fixture) csv(t *testing.T, kind string) (name string, out []byte) {
	t.Helper()
	ctx := context.Background()
	var buf bytes.Buffer
	if err := db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		name, err = reports.WriteCSV(ctx, tx, f.event, kind, &buf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return name, buf.Bytes()
}
