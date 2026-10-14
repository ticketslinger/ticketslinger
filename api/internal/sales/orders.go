package sales

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// are all ErrNotFound, and the token is checked in constant time.
func (s *Service) Order(ctx context.Context, code, token string) (OrderView, error) {
	code = strings.ToUpper(code)
	if !orderCode.MatchString(code) || token == "" {
		return OrderView{}, catalogue.ErrNotFound
	}
	var org *uuid.UUID
	if err := s.Pool.QueryRow(ctx, `select order_scope($1)`, code).Scan(&org); err != nil {
		return OrderView{}, err
	}
	if org == nil {
		return OrderView{}, catalogue.ErrNotFound
	}
	var out OrderView
	err := system.Run(ctx, s.Pool, *org, func(tx pgx.Tx) error {
		var id uuid.UUID
		var hash []byte
		e := &out.Event
		err := tx.QueryRow(ctx, `select o.id, o.magic_token_hash, o.code, o.status, o.email, o.subtotal_cents, o.discount_cents, o.tax_cents,
				o.fee_cents, o.total_cents, e.name, e.timezone, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name
			from orders o join events e on e.id = o.event_id join organizers g on g.id = o.organizer_id where o.code = $1`, code).
			Scan(&id, &hash, &out.Code, &out.Status, &out.Email, &out.SubtotalCents, &out.DiscountCents, &out.TaxCents,
				&out.FeeCents, &out.TotalCents, &e.Name, &e.Timezone, &e.VenueName, &e.VenueAddress, &e.StartsAt, &e.EndsAt, &e.Organizer)
		if errors.Is(err, pgx.ErrNoRows) {
			return catalogue.ErrNotFound
		}
		if err != nil {
			return err
		}
		given := sha256.Sum256([]byte(token))
		if len(hash) != len(given) || subtle.ConstantTimeCompare(hash, given[:]) != 1 {
			return catalogue.ErrNotFound
		}
		rows, _ := tx.Query(ctx, `select t.id, tt.name, t.attendee_name, t.status, t.secret
			from tickets t join ticket_types tt on tt.id = t.ticket_type_id where t.order_id = $1 order by t.id`, id)
		var t OrderTicket
		var secret []byte
		out.Tickets = []OrderTicket{}
		_, err = pgx.ForEachRow(rows, []any{&t.ID, &t.TicketTypeName, &t.AttendeeName, &t.Status, &secret}, func() error {
			t.QR = base64.RawURLEncoding.EncodeToString(secret)
			out.Tickets = append(out.Tickets, t)
			return nil
		})
		return err
	})
	return out, err
}

// RefundPayment handles RefundPaymentJob: a full refund, the platform's fee
// included, of a cancelled order's payment, then the order marked refunded
// and the buyer told. Stripe's idempotency key is the order's, so a crash
// between the refund and our write can't refund twice, and an order that is
// already refunded is left alone.
func RefundPayment(s *Service) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j refundJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		var status, intent, account string
		var payment uuid.UUID
		var amount int64
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select o.status, p.id, p.payment_intent_id, p.stripe_account_id, p.amount_cents
				from orders o join payments p on p.order_id = o.id where o.id = $1`, j.Order).Scan(&status, &payment, &intent, &account, &amount)
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
		refund, err := s.Payments.Refund(ctx, plugin.RefundInput{Account: account, PaymentIntentID: intent, RefundApplicationFee: true,
			IdempotencyKey: "refund-" + j.Order.String()})
		if err != nil {
			return err
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
				values ($1, $2, $3, true, $4, 'requested', 'late_payment')`, j.Organizer, payment, amount, refund); err != nil {
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
}

// dollars formats cents as $26.00.
func dollars(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
