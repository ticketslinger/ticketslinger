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

// OrderView is the buyer's order page.
type OrderView struct {
	Code, Status, Email                                          string
	Event                                                        OrderEvent
	SubtotalCents, DiscountCents, TaxCents, FeeCents, TotalCents int64
	Tickets                                                      []OrderTicket
}

type OrderEvent struct {
	Name, Timezone, VenueName, VenueAddress, Organizer string
	StartsAt, EndsAt                                   time.Time
}

type OrderTicket struct {
	ID                                   uuid.UUID
	TicketTypeName, AttendeeName, Status string
	QR                                   string // base64url of the ticket's secret, 22 characters
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
		&e.Name, &e.Timezone, &e.VenueName, &e.VenueAddress, &e.StartsAt, &e.EndsAt, &e.Organizer, &tickets)
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
		Secret         string    `json:"secret"` // hex
	}
	if err := json.Unmarshal(tickets, &rows); err != nil {
		return OrderView{}, err
	}
	out.Tickets = make([]OrderTicket, len(rows))
	for i, r := range rows {
		secret, err := hex.DecodeString(r.Secret)
		if err != nil {
			return OrderView{}, err
		}
		out.Tickets[i] = OrderTicket{ID: r.ID, TicketTypeName: r.TicketTypeName, AttendeeName: r.AttendeeName, Status: r.Status,
			QR: base64.RawURLEncoding.EncodeToString(secret)}
	}
	return out, nil
}

// OrderSummary is one row of an event's order list.
type OrderSummary struct {
	ID                           uuid.UUID
	Code, Email, Status, FeeMode string
	TicketCount                  int
	TotalCents, FeeCents         int64
	CreatedAt                    time.Time
	PaidAt                       *time.Time
}

// ListOrders is the event's orders, newest first, pending ones left out. It
// runs in the caller's organizer transaction: row-level security keeps it to
// that organizer, so no role switch.
func ListOrders(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]OrderSummary, error) {
	if _, err := catalogue.GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select o.id, o.code, o.email::text, o.status, (select count(*) from tickets t where t.order_id = o.id),
			o.total_cents, o.fee_cents, o.fee_mode, o.created_at, o.paid_at
		from orders o where o.event_id = $1 and o.status <> 'pending' order by o.created_at desc, o.id desc`, event)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (o OrderSummary, err error) {
		err = r.Scan(&o.ID, &o.Code, &o.Email, &o.Status, &o.TicketCount, &o.TotalCents, &o.FeeCents, &o.FeeMode, &o.CreatedAt, &o.PaidAt)
		return
	})
}
