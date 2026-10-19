// Package reports adds up what an event sold, refunded and kept, and exports
// its orders, attendees and check-ins as CSV. Everything runs in the caller's
// organizer transaction, so row-level security keeps it to that organizer.
package reports

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// Report is an event's figures. Money counts the orders the Orders tab lists
// (paid, confirmed free, or paid late after they were cancelled), so its
// totals equal that tab's. RefundedCents is every refund of theirs that
// hasn't failed, gone through or still on its way, and PlatformFeeCents is
// our fee on their succeeded payments less the fee that has actually gone
// back: the same rules as the order list. NetCents is what's left: gross −
// refunded − platform fee, before Stripe's own fees. Sold and CheckedIn count
// valid tickets.
type Report struct {
	GrossCents, RefundedCents, PlatformFeeCents, TaxCents, NetCents int64
	Orders, Sold, CheckedIn                                         int
	TicketTypes                                                     []TypeLine
	Quotas                                                          []QuotaLine
	SalesByDay                                                      []DayLine
	PromoCodes                                                      []PromoLine
}

type TypeLine struct {
	ID              uuid.UUID
	Name            string
	Sold, CheckedIn int
}

type QuotaLine struct {
	ID              uuid.UUID
	Name            string
	Size, Remaining int
}

// DayLine is one day of sales by paid_at (a late payment, which has none, by
// when it was placed), Day "YYYY-MM-DD" in the event's time zone. Tickets
// counts every ticket the day's orders bought.
type DayLine struct {
	Day             string
	Orders, Tickets int
	GrossCents      int64
}

type PromoLine struct {
	Code          string
	UsedCount     int
	MaxUses       *int
	DiscountCents int64
}

// Event reads event's report; catalogue.ErrNotFound if the scoped organizer
// has no such event. It is one statement, so every figure comes from the
// same snapshot whatever the transaction's isolation level.
func Event(ctx context.Context, tx pgx.Tx, event uuid.UUID) (Report, error) {
	ev, err := catalogue.GetEvent(ctx, tx, event)
	if err != nil {
		return Report{}, err
	}
	var r Report
	err = tx.QueryRow(ctx, `with o as (`+listed+`),
		pay as (select p.id, p.application_fee_cents, p.status from payments p join o on o.id = p.order_id),
		ref as (select r.amount_cents, r.fee_cents, r.status, r.fee_refunded_at from refunds r join pay on pay.id = r.payment_id),
		tk as (select ticket_type_id, order_id, status, checked_in_at from tickets where event_id = $1)
		select
			coalesce((select sum(total_cents) from o), 0)::bigint,
			coalesce((select sum(tax_cents) from o), 0)::bigint,
			(select count(*) from o)::int,
			coalesce((select sum(amount_cents) from ref where status <> 'failed'), 0)::bigint,
			coalesce((select sum(application_fee_cents) from pay where status = 'succeeded'), 0)::bigint
				- coalesce((select sum(fee_cents) from ref where fee_refunded_at is not null), 0)::bigint,
			(select count(*) from tk where status = 'valid')::int,
			(select count(*) from tk where status = 'valid' and checked_in_at is not null)::int,
			(select coalesce(json_agg(json_build_object('ID', x.id, 'Name', x.name, 'Sold', x.sold, 'CheckedIn', x.checked_in)
					order by x.position, x.id), '[]')
				from (select tt.id, tt.name, tt.position, count(t.*) filter (where t.status = 'valid') as sold,
						count(t.*) filter (where t.status = 'valid' and t.checked_in_at is not null) as checked_in
					from ticket_types tt left join tk t on t.ticket_type_id = tt.id
					where tt.event_id = $1 group by tt.id) x),
			(select coalesce(json_agg(json_build_object('ID', id, 'Name', name, 'Size', size,
					'Remaining', greatest(size - quota_used(id, null), 0)) order by created_at, id), '[]')
				from quotas where event_id = $1),
			(select coalesce(json_agg(json_build_object('Day', x.day, 'Orders', x.orders, 'Tickets', x.tickets, 'GrossCents', x.gross)
					order by x.day), '[]')
				from (select to_char(coalesce(o.paid_at, o.created_at) at time zone $2, 'YYYY-MM-DD') as day, count(*) as orders,
						sum((select count(*) from tk where tk.order_id = o.id)) as tickets, sum(o.total_cents) as gross
					from o group by day) x),
			(select coalesce(json_agg(json_build_object('Code', x.code, 'UsedCount', x.used_count, 'MaxUses', x.max_uses,
					'DiscountCents', x.discount) order by x.created_at, x.id), '[]')
				from (select p.id, p.code, p.used_count, p.max_uses, p.created_at, coalesce(sum(o.discount_cents), 0) as discount
					from promo_codes p left join o on o.promo_code_id = p.id
					where p.event_id = $1 group by p.id) x)`, event, ev.Timezone).
		Scan(&r.GrossCents, &r.TaxCents, &r.Orders, &r.RefundedCents, &r.PlatformFeeCents, &r.Sold, &r.CheckedIn,
			&r.TicketTypes, &r.Quotas, &r.SalesByDay, &r.PromoCodes)
	if err != nil {
		return Report{}, err
	}
	r.NetCents = r.GrossCents - r.RefundedCents - r.PlatformFeeCents
	return r, nil
}

// listed is the orders of event $1 that the order list shows: ones that took
// money or were confirmed free. Pending ones, and ones abandoned before
// payment, are left out.
const listed = `select o.id, o.code, o.status, o.email, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.fee_mode,
	o.total_cents, o.promo_code_id, o.created_at, o.paid_at from orders o where o.event_id = $1 and o.status <> 'pending'
	and (o.paid_at is not null or exists (select 1 from payments p where p.order_id = o.id and p.status = 'succeeded'))`
