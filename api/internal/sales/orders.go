package sales

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
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

// RefundPayment handles RefundPaymentJob: a full refund, the platform's fee
// included, of a cancelled order's payment, then the order marked refunded
// and the buyer told. Stripe's idempotency key is the order's
// (refund-<order>-<attempt>), so a crash between the refund and our write
// can't refund twice, and an order that is already refunded is left alone.
// Only a definite refusal moves the order to its next key (Stripe replays a
// refusal under one key for 24 hours). A failure on the job's last try is
// raised: an ERROR, and a system row in the organizer's audit log.
func RefundPayment(s *Service) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j refundJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		err := s.refund(ctx, j)
		// A shutdown (cancel, not the job's own timeout) hands the job back
		// to the queue: not stuck, so nothing to raise.
		if err != nil && jobs.Attempt(ctx) >= jobs.MaxAttempts && !errors.Is(ctx.Err(), context.Canceled) {
			s.refundStuck(ctx, j, err)
		}
		return err
	}
}

// refundStuck raises a refund the queue is about to park. The buyer was
// charged and their order cancelled, so someone has to look. Nothing here
// names the buyer or carries a secret: the provider's error is code, type
// and request id only.
func (s *Service) refundStuck(ctx context.Context, j refundJob, cause error) {
	slog.ErrorContext(ctx, "refund stuck: the buyer was charged and nothing will retry it", "organizer", j.Organizer, "order", j.Order, "err", cause)
	ctx = context.WithoutCancel(ctx)
	err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{Organizer: j.Organizer, Actor: audit.Actor{System: true}, Action: "order.refund_stuck",
			EntityType: "order", EntityID: j.Order, After: map[string]string{"error": cause.Error()}})
	})
	if err != nil {
		slog.ErrorContext(ctx, "refund stuck: audit row not written", "order", j.Order, "err", err)
	}
}

func (s *Service) refund(ctx context.Context, j refundJob) error {
	var status, intent, account string
	var payment uuid.UUID
	var amount int64
	var attempt int
	err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select o.status, o.refund_attempt, p.id, p.payment_intent_id, p.stripe_account_id, p.amount_cents
			from orders o join payments p on p.order_id = o.id where o.id = $1`, j.Order).Scan(&status, &attempt, &payment, &intent, &account, &amount)
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && status != "cancelled" {
		return nil // nothing paid, or refunded already
	}
	if err != nil {
		return err
	}
	if s.Payments == nil {
		return errors.New("sales: a refund is due but there's no payment provider")
	}
	refund, err := s.Payments.Refund(ctx, plugin.RefundInput{Account: account, PaymentIntentID: intent, AmountCents: amount,
		IdempotencyKey: fmt.Sprintf("refund-%s-%d", j.Order, attempt)})
	var refundID *string // unknown when Stripe says it's already refunded
	switch {
	case errors.Is(err, plugin.ErrAlreadyRefunded):
	case errors.Is(err, plugin.ErrRefused):
		// Stripe said no and will say it again under this key: the next try uses the next one.
		if berr := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update orders set refund_attempt = refund_attempt + 1 where id = $1 and refund_attempt = $2`, j.Order, attempt)
			return err
		}); berr != nil {
			return errors.Join(err, berr)
		}
		return err
	case err != nil:
		return err
	default:
		refundID = &refund.ID
	}
	return system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
		var email, event string
		if err := tx.QueryRow(ctx, `select o.status, o.email, e.name from orders o join events e on e.id = o.event_id
			where o.id = $1 for update of o`, j.Order).Scan(&status, &email, &event); err != nil {
			return err
		}
		if status != "cancelled" {
			return nil // another run got here first
		}
		if _, err := tx.Exec(ctx, `insert into refunds (organizer_id, payment_id, amount_cents, fee_refunded, stripe_refund_id, status, reason)
			values ($1, $2, $3, true, $4, 'requested', 'late_payment')`, j.Organizer, payment, amount, refundID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update orders set status = 'refunded' where id = $1`, j.Order); err != nil {
			return err
		}
		// No link in it: nothing in this job's payload is a key to anything.
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": email, "subject": "Your refund for " + event,
			"text": "Sorry, we couldn't keep your tickets for " + event + ". By the time your payment reached us, they weren't available any more, " +
				"so we've refunded you in full: " + dollars(amount) + " back to your card. It usually shows up in 5 to 10 days."})
	})
}

// dollars formats cents as $26.00.
func dollars(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
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
