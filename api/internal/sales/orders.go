package sales

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// OrderView is the buyer's order page. RefundedCents adds up the refunds
// that haven't failed; ReturnedCents the ones that went through and that the
// buyer's bank then sent back.
type OrderView struct {
	Code, Status, Email                                          string
	Event                                                        OrderEvent
	SubtotalCents, DiscountCents, TaxCents, FeeCents, TotalCents int64
	RefundedCents, ReturnedCents                                 int64
	Tickets                                                      []OrderTicket
}

type OrderEvent struct {
	Name, Timezone, VenueName, VenueAddress, Organizer, Status string
	StartsAt, EndsAt                                           time.Time
}

type OrderTicket struct {
	ID                                   uuid.UUID
	TicketTypeName, AttendeeName, Status string
	Refunded                             bool   // void because it was refunded
	QR                                   string // base64url of the ticket's secret, 22 characters; "" for a void ticket
}

var orderCode = regexp.MustCompile(`^TS-[0-9A-HJKMNP-TV-Z]{5}$`)

// Order is the order with this code, for whoever holds its link's token.
// An unknown code, an order without a link yet (pending) and a wrong token
// are all ErrNotFound, and each is one statement (order_by_link), the same as
// a hit: the time an answer takes doesn't say whether the code exists.
func (s *Service) Order(ctx context.Context, code, token string) (OrderView, error) {
	if !orderCode.MatchString(strings.ToUpper(code)) || token == "" {
		return OrderView{}, catalogue.ErrNotFound // no query: the format is public
	}
	hash := sha256.Sum256([]byte(token))
	var out OrderView
	var tickets []byte
	e := &out.Event
	err := s.Pool.QueryRow(ctx, `select * from order_by_link($1, $2)`, code, hash[:]).Scan(&out.Code, &out.Status, &out.Email,
		&out.SubtotalCents, &out.DiscountCents, &out.TaxCents, &out.FeeCents, &out.TotalCents,
		&e.Name, &e.Timezone, &e.VenueName, &e.VenueAddress, &e.StartsAt, &e.EndsAt, &e.Organizer, &e.Status,
		&out.RefundedCents, &out.ReturnedCents, &tickets)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderView{}, catalogue.ErrNotFound
	}
	if err != nil {
		return OrderView{}, err
	}
	var rows []struct {
		ID             uuid.UUID `json:"id"`
		TicketTypeName string    `json:"ticket_type_name"`
		AttendeeName   string    `json:"attendee_name"`
		Status         string    `json:"status"`
		Refunded       bool      `json:"refunded"`
		Secret         *string   `json:"secret"` // hex; null for a void ticket
	}
	if err := json.Unmarshal(tickets, &rows); err != nil {
		return OrderView{}, err
	}
	out.Tickets = make([]OrderTicket, len(rows))
	for i, r := range rows {
		out.Tickets[i] = OrderTicket{ID: r.ID, TicketTypeName: r.TicketTypeName, AttendeeName: r.AttendeeName, Status: r.Status,
			Refunded: r.Refunded}
		if r.Secret != nil {
			secret, err := hex.DecodeString(*r.Secret)
			if err != nil {
				return OrderView{}, err
			}
			out.Tickets[i].QR = base64.RawURLEncoding.EncodeToString(secret)
		}
	}
	return out, nil
}

// OrderSummary is one row of an event's order list. TicketCount counts the
// tickets still valid; RefundedCents adds up every refund that hasn't
// failed, whether it has gone through or is still on its way.
// FeeRefundedCents is the fee that has actually gone back: a fee refund still
// to come, or stuck, isn't counted.
type OrderSummary struct {
	ID                              uuid.UUID
	Code, Email, Status, FeeMode    string
	TicketCount                     int
	TotalCents, FeeCents            int64
	RefundedCents, FeeRefundedCents int64
	CreatedAt                       time.Time
	PaidAt                          *time.Time
}

// orderSummarySelect lists the orders of event $1 that took money or were
// confirmed free; ones still pending, or abandoned before payment, are left out.
const orderSummarySelect = `select o.id, o.code, o.email::text, o.status,
		(select count(*) from tickets t where t.order_id = o.id and t.status = 'valid'),
		o.total_cents, o.fee_cents, o.fee_mode, o.created_at, o.paid_at,
		coalesce((select sum(r.amount_cents) from refunds r join payments p on p.id = r.payment_id where p.order_id = o.id and r.status <> 'failed'), 0),
		coalesce((select sum(r.fee_cents) from refunds r join payments p on p.id = r.payment_id where p.order_id = o.id and r.fee_refunded_at is not null), 0)
	from orders o
	where o.event_id = $1 and o.status <> 'pending'
	  and (o.paid_at is not null or exists (select 1 from payments p where p.order_id = o.id and p.status = 'succeeded'))`

func scanOrderSummary(r pgx.CollectableRow) (o OrderSummary, err error) {
	err = r.Scan(&o.ID, &o.Code, &o.Email, &o.Status, &o.TicketCount, &o.TotalCents, &o.FeeCents, &o.FeeMode, &o.CreatedAt, &o.PaidAt,
		&o.RefundedCents, &o.FeeRefundedCents)
	return o, err
}

// ListOrders is the event's orders, newest first. It runs in the caller's
// organizer transaction: row-level security keeps it to that organizer, so
// no role switch.
func ListOrders(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]OrderSummary, error) {
	if _, err := catalogue.GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, orderSummarySelect+` order by o.created_at desc, o.id desc`, event)
	return pgx.CollectRows(rows, scanOrderSummary)
}
