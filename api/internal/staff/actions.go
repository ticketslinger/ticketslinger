package staff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
)

var (
	// ErrAlreadySuspended: suspend on an organizer that already is.
	ErrAlreadySuspended = errors.New("staff: already suspended")
	// ErrNotSuspended: lift or refund_event on an organizer that isn't suspended.
	ErrNotSuspended = errors.New("staff: not suspended")
	// ErrBusy: the organizer row stayed locked past lock_timeout; try again.
	ErrBusy = errors.New("staff: organizer busy")
)

// Action is one staff write: a suspension, a lift, or an event refund.
type Action struct {
	ID            uuid.UUID
	Kind          string
	OrganizerSlug string
	Event         *uuid.UUID
}

// RefundResult is what a refund_event action did; staff_actions.result
// holds it as JSON.
type RefundResult struct {
	Orders        int             `json:"orders"`
	PendingOrders int             `json:"pending_orders"`
	AmountCents   int64           `json:"amount_cents"`
	FeeCents      int64           `json:"fee_cents"`
	FailedOrders  []RefusedRefund `json:"failed_orders"`
}

// RefusedRefund is an order the refund left out, for someone to refund by hand.
type RefusedRefund struct {
	Order  uuid.UUID `json:"order_id"`
	Reason string    `json:"reason"`
}

// ActionLog is a staff action as the staff log shows it.
type ActionLog struct {
	ID         uuid.UUID
	Kind       string
	Event      *uuid.UUID
	Reason     string
	At         time.Time
	FinishedAt *time.Time
	Result     *RefundResult // refund_event, once finished
	// Failed: refund_event whose background refund failed or panicked
	// since this process started. It stays unfinished, to take again.
	Failed bool
}

// Purpose is what a staff action's passkey touch is for, so a touch made
// for one kind, organizer or event can't be spent on another.
func Purpose(kind string, org uuid.UUID, event *uuid.UUID) string {
	p := kind + ":" + org.String()
	if event != nil {
		p += ":" + event.String()
	}
	return p
}

// The alert's subject verb, per kind.
var actedOn = map[string]string{
	"suspend":      "suspended",
	"lift":         "lifted the suspension of",
	"refund_event": "refunded an event of",
}

// ownerSteps closes the alert for a suspension or a refund (spec §13.8):
// nothing on the Stripe side is automated.
const ownerSteps = `
What to do on the Stripe side, in this order:

1. Refund first (the refund action in staff mode), and wait until the refunds show as succeeded there.
2. Report the account to Stripe support from the Stripe dashboard (Connect, then the account, then contact support), with the organizer's web address and the reason. Stripe decides about their account.
3. Only after the refunds have settled, and only if we never want them back, remove the account from the platform in the Stripe dashboard. This can't be undone from our side.
`

// Act takes one staff action on sess's organizer: staff_act re-checks the
// staff session and records the action, and the alert email is queued in
// the same transaction. The passkey touch is checked before this is called.
// A refund_event is then carried out in the background, in a second
// transaction, and Act returns without waiting: listStaffSessions shows it
// finished, with its result. If it fails, the action stays recorded and
// unfinished, and staff take it again.
func (s *Service) Act(ctx context.Context, sess Session, kind string, event *uuid.UUID, reason string) (Action, error) {
	if s.alertTo == "" {
		return Action{}, ErrAlertsOff
	}
	out := Action{Kind: kind, OrganizerSlug: sess.OrganizerSlug, Event: event}
	err := db.WithUser(ctx, s.pool, sess.UserID, func(tx pgx.Tx) error {
		// staff_act's FOR UPDATE queues every new member write behind it
		// while it waits, so it gives up after 5 seconds instead.
		if _, err := tx.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `select staff_act($1, $2, $3, $4, $5)`, sess.ID, sess.LoginSession, kind, event, reason).Scan(&out.ID); err != nil {
			return err
		}
		var email, name, slug string
		var at time.Time
		// staff_actions.at is now(), too.
		if err := tx.QueryRow(ctx, `select u.email, ss.organizer_name, coalesce(staff_session_slug(ss.id), ss.organizer_slug::text), now()
			  from staff_sessions ss join users u on u.id = ss.user_id where ss.id = $1`, sess.ID).Scan(&email, &name, &slug, &at); err != nil {
			return err
		}
		out.OrganizerSlug = slug
		text := fmt.Sprintf("%s %s %s (%s) at %s.\n\nTheir reason: %s\n", email, actedOn[kind], name, slug, at.UTC().Format("2 Jan 2006, 15:04 MST"), reason)
		if kind != "lift" {
			text += ownerSteps
		}
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{"to": s.alertTo, "subject": "Staff " + actedOn[kind] + " " + slug, "text": text})
	})
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch {
		case pg.Code == "55000" && pg.Message == "already_suspended":
			return Action{}, ErrAlreadySuspended
		case pg.Code == "55000" && pg.Message == "not_suspended":
			return Action{}, ErrNotSuspended
		case pg.Code == "P0002":
			return Action{}, catalogue.ErrNotFound
		case pg.Code == "42501":
			return Action{}, ErrNotAllowed
		case pg.Code == "55P03":
			return Action{}, ErrBusy
		}
	}
	if err != nil {
		return Action{}, err
	}
	slog.WarnContext(ctx, "staff_action", "action", out.ID, "kind", kind, "staff_session", sess.ID, "user", sess.UserID, "organizer", sess.OrganizerID, "event", event)
	if kind != "refund_event" {
		return out, nil
	}
	// Not cancelled with the request, and not waited for: a big event
	// outlasts nginx's 30 seconds. It stops with the server (StopWith) and
	// after 10 minutes, rolling back and staying unfinished either way.
	// ponytail: move it to a job if an event ever needs longer.
	s.refunds.Add(1)
	go func() {
		defer s.refunds.Done()
		failed := func(err any) {
			slog.Error("staff_action: refund not carried out", "action", out.ID, "organizer", sess.OrganizerID, "event", event, "err", err)
			s.mu.Lock()
			s.failed[out.ID] = true
			s.mu.Unlock()
		}
		// A panic here would take every tenant's requests down with it (jobs.go does the same).
		defer func() {
			if v := recover(); v != nil {
				failed(fmt.Sprintf("panic: %v\n%s", v, debug.Stack()))
			}
		}()
		rctx, cancel := context.WithTimeout(s.life, 10*time.Minute)
		defer cancel()
		if _, err := s.refund(rctx, sess.OrganizerID, out.ID); err != nil {
			failed(err) // already_done or not_suspended included: ours to look at
		}
	}()
	return out, nil
}

// Wait returns once every refund Act started has finished.
func (s *Service) Wait() { s.refunds.Wait() }

// StopWith ends background refunds when ctx is done (the server shutting
// down): they roll back and stay unfinished. Call it before serving.
func (s *Service) StopWith(ctx context.Context) { s.life = ctx }

// markFailed sets Failed on every action whose background refund failed.
func (s *Service) markFailed(list []SessionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range list {
		for j := range list[i].Actions {
			list[i].Actions[j].Failed = s.failed[list[i].Actions[j].ID]
		}
	}
}
