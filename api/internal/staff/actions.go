package staff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// ErrAlreadyDone: a refund_event action that has already been carried out.
	ErrAlreadyDone = errors.New("staff: already done")
)

// Action is one staff write: a suspension, a lift, or an event refund.
type Action struct {
	ID            uuid.UUID
	Kind          string
	OrganizerSlug string
	Event         *uuid.UUID
	Result        *RefundResult
}

// RefundResult is what a refund_event action did.
type RefundResult struct {
	Orders, PendingOrders int
	AmountCents, FeeCents int64
}

// ActionLog is a staff action as the staff log shows it.
type ActionLog struct {
	ID         uuid.UUID
	Kind       string
	Event      *uuid.UUID
	Reason     string
	At         time.Time
	FinishedAt *time.Time
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
// A refund_event is then carried out in a second transaction; if that one
// fails, the action stays recorded and unfinished, and staff start again.
func (s *Service) Act(ctx context.Context, sess Session, kind string, event *uuid.UUID, reason string) (Action, error) {
	if s.alertTo == "" {
		return Action{}, ErrAlertsOff
	}
	out := Action{Kind: kind, OrganizerSlug: sess.OrganizerSlug, Event: event}
	err := db.WithUser(ctx, s.pool, sess.UserID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select staff_act($1, $2, $3, $4, $5)`, sess.ID, sess.LoginSession, kind, event, reason).Scan(&out.ID); err != nil {
			return err
		}
		var email, name, slug string
		var at time.Time
		// staff_actions.at is now(), too.
		if err := tx.QueryRow(ctx, `select u.email, ss.organizer_name, ss.organizer_slug::text, now()
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
	if err == nil {
		slog.WarnContext(ctx, "staff_action", "action", out.ID, "kind", kind, "staff_session", sess.ID, "user", sess.UserID, "organizer", sess.OrganizerID, "event", event)
		if kind == "refund_event" {
			var r RefundResult
			r, err = s.refund(ctx, sess.OrganizerID, out.ID)
			out.Result = &r
			if err != nil {
				slog.ErrorContext(ctx, "staff_action: refund not carried out", "action", out.ID, "organizer", sess.OrganizerID, "event", event, "err", err)
			}
		}
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch {
		case pg.Code == "55000" && pg.Message == "already_done":
			return Action{}, ErrAlreadyDone
		case pg.Code == "55000" && pg.Message == "already_suspended":
			return Action{}, ErrAlreadySuspended
		case pg.Code == "55000" && pg.Message == "not_suspended":
			return Action{}, ErrNotSuspended
		case pg.Code == "P0002":
			return Action{}, catalogue.ErrNotFound
		case pg.Code == "42501":
			return Action{}, ErrNotAllowed
		}
	}
	if err != nil {
		return Action{}, err
	}
	return out, nil
}
