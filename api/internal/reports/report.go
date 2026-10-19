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

// Report is an event's figures. Money counts paid orders. RefundedCents is
// every refund that hasn't failed, gone through or still on its way, and
// PlatformFeeCents is our fee on succeeded payments less the fee that has
// actually gone back: the same rules as the order list. NetCents is what's
// left: gross − refunded − platform fee, before Stripe's own fees. Sold and
// CheckedIn count valid tickets.
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

// DayLine is one day of sales by paid_at, Day "YYYY-MM-DD" in the event's
// time zone. Tickets counts every ticket the day's orders bought.
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
// has no such event.
func Event(ctx context.Context, tx pgx.Tx, event uuid.UUID) (Report, error) {
	ev, err := catalogue.GetEvent(ctx, tx, event)
	if err != nil {
		return Report{}, err
	}
	var r Report
	err = tx.QueryRow(ctx, `select
			coalesce((select sum(total_cents) from orders where event_id = $1 and paid_at is not null), 0)::bigint,
			coalesce((select sum(tax_cents) from orders where event_id = $1 and paid_at is not null), 0)::bigint,
			(select count(*) from orders where event_id = $1 and paid_at is not null),
			coalesce((select sum(r.amount_cents) from refunds r join payments p on p.id = r.payment_id join orders o on o.id = p.order_id
				where o.event_id = $1 and r.status <> 'failed'), 0)::bigint,
			coalesce((select sum(p.application_fee_cents) from payments p join orders o on o.id = p.order_id
				where o.event_id = $1 and p.status = 'succeeded'), 0)::bigint
			- coalesce((select sum(r.fee_cents) from refunds r join payments p on p.id = r.payment_id join orders o on o.id = p.order_id
				where o.event_id = $1 and r.fee_refunded_at is not null), 0)::bigint,
			(select count(*) from tickets where event_id = $1 and status = 'valid'),
			(select count(*) from tickets where event_id = $1 and status = 'valid' and checked_in_at is not null)`, event).
		Scan(&r.GrossCents, &r.TaxCents, &r.Orders, &r.RefundedCents, &r.PlatformFeeCents, &r.Sold, &r.CheckedIn)
	if err != nil {
		return Report{}, err
	}
	r.NetCents = r.GrossCents - r.RefundedCents - r.PlatformFeeCents

	rows, _ := tx.Query(ctx, `select tt.id, tt.name, count(t.id) filter (where t.status = 'valid'),
			count(t.id) filter (where t.status = 'valid' and t.checked_in_at is not null)
		from ticket_types tt left join tickets t on t.ticket_type_id = tt.id
		where tt.event_id = $1 group by tt.id order by tt.position, tt.id`, event)
	if r.TicketTypes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TypeLine]); err != nil {
		return Report{}, err
	}
	rows, _ = tx.Query(ctx, `select id, name, size, greatest(size - quota_used(id, null), 0)::int
		from quotas where event_id = $1 order by created_at, id`, event)
	if r.Quotas, err = pgx.CollectRows(rows, pgx.RowToStructByPos[QuotaLine]); err != nil {
		return Report{}, err
	}
	rows, _ = tx.Query(ctx, `select to_char(o.paid_at at time zone $2, 'YYYY-MM-DD') as day, count(*),
			coalesce(sum((select count(*) from tickets t where t.order_id = o.id)), 0)::int, sum(o.total_cents)::bigint
		from orders o where o.event_id = $1 and o.paid_at is not null group by day order by day`, event, ev.Timezone)
	if r.SalesByDay, err = pgx.CollectRows(rows, pgx.RowToStructByPos[DayLine]); err != nil {
		return Report{}, err
	}
	rows, _ = tx.Query(ctx, `select p.code, p.used_count, p.max_uses,
			coalesce(sum(o.discount_cents) filter (where o.paid_at is not null), 0)::bigint
		from promo_codes p left join orders o on o.promo_code_id = p.id
		where p.event_id = $1 group by p.id order by p.created_at, p.id`, event)
	if r.PromoCodes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PromoLine]); err != nil {
		return Report{}, err
	}
	return r, nil
}
