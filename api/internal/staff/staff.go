// Package staff starts and ends staff mode: a TicketSlinger staff member
// reading one organizer for at most an hour, with a written reason, an
// audit row in that organizer and an alert email for every start.
package staff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
)

var (
	// ErrAlertsOff: TS_MAIL_ALERT_TO isn't set, and staff mode never starts unannounced.
	ErrAlertsOff = errors.New("staff: alert address not set")
	// ErrNotAllowed: not live staff, or the login session isn't theirs and MFA.
	ErrNotAllowed = errors.New("staff: not allowed")
	// ErrNoOrganizer: no organizer has that slug.
	ErrNoOrganizer = errors.New("staff: no such organizer")
	// ErrNoSession: the login session has no live staff session.
	ErrNoSession = errors.New("staff: no live staff session")
)

// Session is a live staff session.
type Session struct {
	ID, UserID, LoginSession, OrganizerID uuid.UUID
	OrganizerSlug, OrganizerName, Reason  string
	ExpiresAt                             time.Time
}

// OrgRef is what staff search shows of an organizer: nothing more.
type OrgRef struct{ Name, Slug string }

// Request is one request made in staff mode.
type Request struct {
	Method, Route string
	At            time.Time
}

// SessionLog is a staff session, live or ended, with its requests and,
// for a staff reader, its actions.
type SessionLog struct {
	ID                                              uuid.UUID
	StaffName, OrganizerSlug, OrganizerName, Reason string
	StartedAt, ExpiresAt                            time.Time
	EndedAt                                         *time.Time
	EndedHow, EndNote                               *string
	Requests                                        []Request
	Actions                                         []ActionLog
}

type Service struct {
	pool    *pgxpool.Pool
	alertTo string
	refund  Refunder
}

// Refunder carries out a refund_event action as the organizer's system
// role (sales.Service.RefundEventForStaff, which this package can't import).
type Refunder func(ctx context.Context, org, action uuid.UUID) (RefundResult, error)

// New returns the service; alertTo is TS_MAIL_ALERT_TO, and without it
// Start refuses. refund carries out refund_event actions.
func New(pool *pgxpool.Pool, alertTo string, refund Refunder) *Service {
	return &Service{pool, alertTo, refund}
}

// AlertsOff reports whether Start would refuse with ErrAlertsOff.
func (s *Service) AlertsOff() bool { return s.alertTo == "" }

// IsStaff reports whether user is live (granted, not revoked) staff.
func (s *Service) IsStaff(ctx context.Context, user uuid.UUID) (bool, error) {
	var ok bool
	err := db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select app_is_staff()`).Scan(&ok)
	})
	return ok, err
}

// Status is IsStaff and Current in one query, for GET /me.
func (s *Service) Status(ctx context.Context, user, loginSession uuid.UUID) (bool, *Session, error) {
	var (
		ok              bool
		id, org         *uuid.UUID
		slug, name, why *string
		expires         *time.Time
	)
	err := db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		// staff_sessions' policy shows rows only to live staff, so a revoked
		// user's session is gone here at once.
		return tx.QueryRow(ctx, `select app_is_staff(), ss.id, ss.organizer_id, ss.organizer_slug::text, ss.organizer_name, ss.reason, ss.expires_at
			  from (select 1) one
			  left join staff_sessions ss on ss.session_id = $1 and ss.user_id = app_user() and ss.organizer_id is not null
			   and ss.ended_at is null and ss.expires_at > now()`, loginSession).
			Scan(&ok, &id, &org, &slug, &name, &why, &expires)
	})
	if err != nil || id == nil {
		return ok, nil, err
	}
	return ok, &Session{ID: *id, UserID: user, LoginSession: loginSession, OrganizerID: *org,
		OrganizerSlug: *slug, OrganizerName: *name, Reason: *why, ExpiresAt: *expires}, nil
}

// Current is the login session's live staff session, or nil.
func (s *Service) Current(ctx context.Context, user, loginSession uuid.UUID) (*Session, error) {
	_, cur, err := s.Status(ctx, user, loginSession)
	return cur, err
}

// Start begins staff mode on orgSlug for user's MFA login session, ending
// any staff session it already had, and queues the alert email in the same
// transaction. The passkey touch is checked before this is called.
func (s *Service) Start(ctx context.Context, user, loginSession uuid.UUID, orgSlug, reason, ip, ua string) (Session, error) {
	if s.alertTo == "" {
		return Session{}, ErrAlertsOff
	}
	out := Session{UserID: user, LoginSession: loginSession}
	err := db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		var addr *string
		if ip != "" {
			addr = &ip
		}
		if err := tx.QueryRow(ctx, `select start_staff_session($1, $2, $3, $4::inet, $5)`, loginSession, orgSlug, reason, addr, ua).Scan(&out.ID); err != nil {
			return err
		}
		// A second statement: the first one's snapshot predates the row.
		var started time.Time
		var email string
		if err := tx.QueryRow(ctx, `select ss.organizer_id, ss.organizer_slug::text, ss.organizer_name, ss.reason, ss.started_at, ss.expires_at, u.email
			  from staff_sessions ss join users u on u.id = ss.user_id where ss.id = $1`, out.ID).
			Scan(&out.OrganizerID, &out.OrganizerSlug, &out.OrganizerName, &out.Reason, &started, &out.ExpiresAt, &email); err != nil {
			return err
		}
		return jobs.Enqueue(ctx, tx, "send_email", map[string]string{
			"to":      s.alertTo,
			"subject": "Staff mode started on " + out.OrganizerSlug,
			"text": fmt.Sprintf("%s started staff mode on %s (%s) at %s.\n\nTheir reason: %s\n\n"+
				"It ends by itself at %s, sooner if they end it or sign out. If you didn't expect this, revoke them on the server now:\n\n    sudo ts-staff revoke %s\n",
				email, out.OrganizerName, out.OrganizerSlug, started.UTC().Format("2 Jan 2006, 15:04 MST"), out.Reason,
				out.ExpiresAt.UTC().Format("15:04 MST"), email),
		})
	})
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pg.Code {
		case "42501":
			return Session{}, ErrNotAllowed
		case "P0002":
			return Session{}, ErrNoOrganizer
		}
	}
	return out, err
}

// End ends the login session's live staff session; how is "staff" (the
// End button) or "logout". ErrNoSession when there is none.
func (s *Service) End(ctx context.Context, user, loginSession uuid.UUID, how, note string) error {
	var n *string
	if note != "" {
		n = &note
	}
	return db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		// One that ran out is left for the sweeper to close as expired.
		var id uuid.UUID
		err := tx.QueryRow(ctx, `select id from staff_sessions where session_id = $1 and user_id = app_user()
			and ended_at is null and expires_at > now()`, loginSession).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `select end_staff_session($1, $2, $3)`, id, how, n)
		return err
	})
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Search finds up to 20 organizers by any part of the name or slug, in any
// case; LIKE's wildcards in q match only themselves. Empty unless user is
// live staff.
func (s *Service) Search(ctx context.Context, user uuid.UUID, q string) ([]OrgRef, error) {
	out := []OrgRef{}
	err := db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select name, slug from staff_search_organizers($1)`, likeEscaper.Replace(q))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[OrgRef])
		return err
	})
	return out, err
}

// List is the newest 100 staff sessions, of every staff member, each with
// its requests in order. Empty unless user is live staff.
func (s *Service) List(ctx context.Context, user uuid.UUID) ([]SessionLog, error) {
	var out []SessionLog
	err := db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) (err error) {
		out, err = sessionLogs(ctx, tx, `users u on u.id = ss.user_id`)
		return err
	})
	return out, err
}

// ForOrganizer is List for the organizer tx is scoped to: its staff
// sessions only, with staff names from app_staff_directory, which never
// gives an email. The filter is explicit: RLS shows a staff reader every
// staff session.
func ForOrganizer(ctx context.Context, tx pgx.Tx) ([]SessionLog, error) {
	return sessionLogs(ctx, tx, `app_staff_directory() u on u.user_id = ss.user_id where ss.organizer_id = app_organizer()`)
}

// sessionLogs reads the newest 100 staff sessions RLS shows, naming staff
// from names (a join clause aliased u with a name column, and any where).
// ponytail: newest 100 and no paging; add a cursor when the log outgrows a page.
func sessionLogs(ctx context.Context, tx pgx.Tx, names string) ([]SessionLog, error) {
	rows, err := tx.Query(ctx, `select ss.id, u.name, ss.organizer_slug::text, ss.organizer_name, ss.reason, ss.started_at, ss.expires_at,
			ss.ended_at, ss.ended_how, ss.end_note,
			coalesce((select jsonb_agg(jsonb_build_object('Method', l.method, 'Route', l.route, 'At', l.at) order by l.at, l.id)
			            from staff_access_log l where l.staff_session_id = ss.id), '[]'),
			coalesce((select jsonb_agg(jsonb_build_object('ID', a.id, 'Kind', a.kind, 'Event', a.event_id, 'Reason', a.reason, 'At', a.at, 'FinishedAt', a.finished_at) order by a.at, a.id)
			            from staff_session_actions(ss.id) a), '[]')
		  from staff_sessions ss join `+names+`
		 order by ss.started_at desc, ss.id desc limit 100`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SessionLog])
	if out == nil {
		out = []SessionLog{}
	}
	return out, err
}

// Log records one request of staffSession before it runs, in its own
// committed transaction. It fails unless the session is live and user's.
func (s *Service) Log(ctx context.Context, user, staffSession uuid.UUID, method, route string) error {
	return db.WithUser(ctx, s.pool, user, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select log_staff_access($1, $2, $3)`, staffSession, method, route)
		return err
	})
}
