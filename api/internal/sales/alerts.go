package sales

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// OrderAlertJob emails one opted-in owner or admin about one new order.
const OrderAlertJob = "order_alert"

// alertCap is how many alerts a member gets a UTC day; claim_order_alerts
// enforces it, and the one numbered this is told it's the last.
const alertCap = 50

type orderAlert struct { // ids and a count, never an address
	Organizer uuid.UUID `json:"organizer_id"`
	Order     uuid.UUID `json:"order_id"`
	User      uuid.UUID `json:"user_id"`
	Nth       int       `json:"nth"`
}

// enqueueOrderAlerts claims today's alerts for the organizer's opted-in
// owners and admins and queues one job for each, in the order's transaction.
func enqueueOrderAlerts(ctx context.Context, tx pgx.Tx, org, order uuid.UUID) error {
	rows, _ := tx.Query(ctx, `select user_id, nth from claim_order_alerts()`)
	type claim struct {
		User uuid.UUID
		Nth  int
	}
	claims, err := pgx.CollectRows(rows, pgx.RowToStructByPos[claim])
	if err != nil {
		return err
	}
	for _, c := range claims {
		if err := jobs.Enqueue(ctx, tx, OrderAlertJob, orderAlert{Organizer: org, Order: order, User: c.User, Nth: c.Nth}); err != nil {
			return err
		}
	}
	return nil
}

// OrderAlert handles OrderAlertJob. A member who is no longer an opted-in
// owner or admin, or an order that's gone, gets nothing. The mail names no
// buyer and no attendee (P7-D7).
func (s *Service) OrderAlert(m mailer.Mailer) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j orderAlert
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		var to string
		a := alertMail{Last: j.Nth >= alertCap}
		var event, slug string
		err := system.Run(ctx, s.Pool, j.Organizer, func(tx pgx.Tx) error {
			var addr *string
			if err := tx.QueryRow(ctx, `select order_alert_recipient($1)`, j.User).Scan(&addr); err != nil {
				return err
			}
			if addr == nil {
				return pgx.ErrNoRows
			}
			to = *addr
			if err := tx.QueryRow(ctx, `select o.code, e.slug, e.name, g.slug, g.name, o.total_cents
				from orders o join events e on e.id = o.event_id join organizers g on g.id = o.organizer_id where o.id = $1`, j.Order).
				Scan(&a.Code, &event, &a.Event, &slug, &a.Organizer, &a.TotalCents); err != nil {
				return err
			}
			rows, _ := tx.Query(ctx, `select tt.name, count(*) from tickets t join ticket_types tt on tt.id = t.ticket_type_id
				where t.order_id = $1 and t.status = 'valid' group by tt.id, tt.name, tt.position order by tt.position, tt.id`, j.Order)
			type line struct {
				Name string
				N    int
			}
			ls, err := pgx.CollectRows(rows, pgx.RowToStructByPos[line])
			for _, l := range ls {
				a.Lines = append(a.Lines, strconv.Itoa(l.N)+" × "+l.Name)
				a.Tickets += l.N
			}
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		base := s.DashboardURL + "/o/" + slug
		a.OrdersURL, a.TeamURL = base+"/events/"+event+"/orders", base+"/team"
		a.OrderURL = a.OrdersURL + "/" + j.Order.String()
		subject, text := orderAlertEmail(a)
		// Outside the transaction, so the quota watch counts the send.
		return jobs.Deliver(ctx, m, emails.Plain(to, subject, text))
	}
}

type alertMail struct {
	Event, Organizer, Code       string
	Lines                        []string // "2 × General", in ticket-type position order
	Tickets                      int
	TotalCents                   int64
	OrderURL, OrdersURL, TeamURL string
	Last                         bool // Nth == 50
}

func orderAlertEmail(a alertMail) (subject, text string) {
	total := "Free"
	if a.TotalCents > 0 {
		total = dollars(a.TotalCents)
	}
	text = plural(a.Tickets, "ticket") + " for " + a.Event + ": " + strings.Join(a.Lines, ", ") + ". Total " + total + ".\n\nSee the order: " + a.OrderURL + "\n\n"
	if a.Last {
		text += "That's the last alert today: we stop at 50 a day so a big on-sale doesn't bury your inbox. Every order is on the Orders tab: " + a.OrdersURL + "\n\n"
	}
	text += "You get these because you turned on order emails for " + a.Organizer + ". Turn them off on the Team page: " + a.TeamURL
	return "New order " + a.Code + " for " + a.Event, text
}
