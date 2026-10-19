package sales

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/emails"
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
//
// The links rotate only after the send succeeds, so a failed send never
// kills a working link. The key names the new links: a retry after a
// send whose rotation didn't commit rebuilds the same links under the same
// key, so the provider drops the copy and the rotation lands.
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
		type due struct {
			ID          uuid.UUID
			Code, Email string
			Version     int
		}
		var name string
		var orders []due
		err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select name from events where id = $1`, event).Scan(&name); err != nil {
				return err
			}
			rows, _ := tx.Query(ctx, `select id, code, email::text, link_version from orders where event_id = $1 and email = $2 and magic_token_hash is not null
				and (not $3 or link_sent_at is null or link_sent_at < statement_timestamp() - interval '10 minutes')
				order by created_at`, event, j.Email, first)
			orders, err = pgx.CollectRows(rows, pgx.RowToStructByPos[due])
			return err
		})
		if err != nil || len(orders) == 0 {
			return err
		}
		// The key names exactly which links this email carries: the provider
		// counts a second body under a used key as already sent, so two
		// different link sets must never share one.
		var lines, pairs []string
		for _, o := range orders {
			lines = append(lines, o.Code+": "+s.orderURL(o.Code, o.ID, o.Version+1))
			pairs = append(pairs, o.ID.String()+":"+strconv.Itoa(o.Version+1))
		}
		slices.Sort(pairs)
		sum := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
		msg := emails.Plain(
			orders[len(orders)-1].Email, // the address on the order, not the spelling typed in the form
			"Your ticket link for "+name,
			"Here's the link to your tickets for "+name+
				". It replaces any link we sent before, so an older one won't open them any more. Keep it to yourself: anyone with it can see your tickets.\n\n"+
				strings.Join(lines, "\n"))
		msg.IdempotencyKey = "ts-job-" + jobs.ID(ctx) + "-l" + hex.EncodeToString(sum[:])[:16]
		err = jobs.Deliver(ctx, m, msg)
		if err != nil {
			return err
		}
		// ponytail: two ceilings. A rotation that fails to commit until the job
		// parks leaves the buyer an email whose links never open (their old link
		// keeps working). Resend keeps a key for 24 hours, so a retry after that
		// sends an identical second copy.
		// The email went out: try to commit even while the worker shuts down.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return system.Run(rctx, s.Pool, org, func(tx pgx.Tx) error {
			for _, o := range orders {
				hash := sha256.Sum256([]byte(orderToken(s.SecretKey, o.ID, o.Version+1)))
				// No row: another request rotated it first, and its link went out.
				if _, err := tx.Exec(rctx, `update orders set link_version = $3, magic_token_hash = $4, link_sent_at = statement_timestamp()
					where id = $1 and link_version = $2`, o.ID, o.Version, o.Version+1, hash[:]); err != nil {
					return err
				}
			}
			return nil
		})
	}
}
