package sales

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// OrderLinksJob mails a buyer fresh links to their orders for one event.
const OrderLinksJob = "order_links"

type linksJob struct {
	Organizer string `json:"organizer_slug"`
	Event     string `json:"event_slug"`
	Email     string `json:"email"`
}

// RequestOrderLinks queues an email of fresh order links for whoever bought
// tickets for this event with this address. It never reads orders, so its
// answer and its timing are the same whether the address bought anything or
// not; the job decides, and writes only to a buyer.
func (s *Service) RequestOrderLinks(ctx context.Context, organizerSlug, eventSlug, email string) error {
	email = strings.TrimSpace(email)
	if !goodEmail(email) {
		return errBadEmail
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return jobs.Enqueue(ctx, tx, OrderLinksJob, linksJob{Organizer: organizerSlug, Event: eventSlug, Email: email})
	})
}

// OrderLinks handles OrderLinksJob. Every order this email has for the
// event gets a new link (the old one stops working), and one email lists
// them. An email with no orders gets nothing: we never write to strangers.
// On a request's first attempt, an order whose link was sent in the last 10
// minutes is skipped, so a flood of requests can't keep killing a working
// link; a retry of the same request isn't a new request.
func (s *Service) OrderLinks(m mailer.Mailer) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var j linksJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return err
		}
		var org, event uuid.UUID
		err := s.Pool.QueryRow(ctx, `select organizer_id, event_id from order_link_scope($1, $2)`, j.Organizer, j.Event).Scan(&org, &event)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		first := jobs.Attempt(ctx) <= 1
		var name, to string
		var lines []string
		err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select name from events where id = $1`, event).Scan(&name); err != nil {
				return err
			}
			type due struct {
				ID          uuid.UUID
				Code, Email string
			}
			rows, _ := tx.Query(ctx, `select id, code, email::text from orders where event_id = $1 and email = $2 and magic_token_hash is not null
				and (not $3 or link_sent_at is null or link_sent_at < statement_timestamp() - interval '10 minutes')
				order by created_at for update`, event, j.Email, first)
			orders, err := pgx.CollectRows(rows, pgx.RowToStructByPos[due])
			if err != nil {
				return err
			}
			for _, o := range orders {
				var v int
				if err := tx.QueryRow(ctx, `update orders set link_version = link_version + 1, link_sent_at = statement_timestamp()
					where id = $1 returning link_version`, o.ID).Scan(&v); err != nil {
					return err
				}
				hash := sha256.Sum256([]byte(orderToken(s.SecretKey, o.ID, v)))
				if _, err := tx.Exec(ctx, `update orders set magic_token_hash = $2 where id = $1`, o.ID, hash[:]); err != nil {
					return err
				}
				lines = append(lines, o.Code+": "+s.orderURL(o.Code, o.ID, v))
				to = o.Email // the address on the order, not the spelling typed in the form
			}
			return nil
		})
		if err != nil || len(lines) == 0 {
			return err
		}
		return m.Send(ctx, to, "Your ticket link for "+name, "Here's the link to your tickets for "+name+
			". It replaces any link we sent before, so an older one won't open them any more. Keep it to yourself: anyone with it can see your tickets.\n\n"+
			strings.Join(lines, "\n"))
	}
}
