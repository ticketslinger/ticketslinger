package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/staff"
)

var (
	errAlertsOff     = &apiError{503, "alerts_off", "Support mode needs an alert address, and this server has none. Set TS_MAIL_ALERT_TO, then try again."}
	errStaffPasskey  = &apiError{403, "passkey_required", "Touch your passkey to start support mode."}
	errNoOrganizer   = &apiError{404, "no_organizer", "No organizer has that web address. Search for it again."}
	errInvalidReason = invalidField("invalid_reason", "reason", "Say why you need to look, in 10 to 500 characters. The organizer's owners will read it.")
)

// staffCaller is the caller of a /staff/* route: live staff, signed in with
// a second factor. Anyone else, API keys included, gets the same 404 as a
// route that doesn't exist, from authenticate before the body is read (the
// generated wrapper still parses query parameters first, so a missing q is
// a 400). The handlers ask again. A staff member on a password-only session
// is an ordinary user.
func (s *server) staffCaller(ctx context.Context) (auth.User, auth.Session, error) {
	a, ok := ctx.Value(authKey).(authed)
	if !ok {
		return auth.User{}, auth.Session{}, unauthorized()
	}
	if a.isKey() || !a.sess.MFA {
		return auth.User{}, auth.Session{}, notFound()
	}
	ok, err := s.Staff.IsStaff(ctx, a.user.ID)
	if err != nil {
		return auth.User{}, auth.Session{}, err
	}
	if !ok {
		return auth.User{}, auth.Session{}, notFound()
	}
	return a.user, a.sess, nil
}

// staffPurpose is what a staff-mode passkey touch is for: starting it, on
// this login session (the challenge row names the session).
const staffPurpose = "start"

func (s *server) BeginStaffSession(ctx context.Context, _ BeginStaffSessionRequestObject) (BeginStaffSessionResponseObject, error) {
	u, sess, err := s.staffCaller(ctx)
	if err != nil {
		return nil, err
	}
	if s.Staff.AlertsOff() {
		return nil, errAlertsOff
	}
	id, opts, err := s.Auth.BeginStaffAssertion(ctx, u.ID, sess.ID, "staff", staffPurpose)
	switch {
	case errors.Is(err, auth.ErrPasskeyRequired):
		return nil, errStaffPasskey
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	m, err := toMap(opts)
	if err != nil {
		return nil, err
	}
	return BeginStaffSession200JSONResponse{ChallengeId: id, Options: m}, nil
}

func toStaffSession(ss staff.Session) StaffSession {
	return StaffSession{Id: ss.ID, OrganizerSlug: ss.OrganizerSlug, OrganizerName: ss.OrganizerName, Reason: ss.Reason, ExpiresAt: ss.ExpiresAt}
}

func (s *server) StartStaffSession(ctx context.Context, req StartStaffSessionRequestObject) (StartStaffSessionResponseObject, error) {
	u, sess, err := s.staffCaller(ctx)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.Body.Reason)
	if n := utf8.RuneCountInString(reason); n < 10 || n > 500 {
		return nil, errInvalidReason
	}
	if utf8.RuneCountInString(req.Body.Organizer) > 40 {
		return nil, invalidField("invalid_slug", "organizer", "That isn't an organizer's web address: they're 40 characters at most.")
	}
	// Before the touch is spent: nothing is written when alerts are off.
	if s.Staff.AlertsOff() {
		return nil, errAlertsOff
	}
	body, err := json.Marshal(req.Body.Credential)
	if err != nil {
		return nil, err
	}
	c := clientFrom(ctx)
	err = s.Auth.VerifyStaffAssertion(ctx, u.ID, sess.ID, req.Body.ChallengeId, "staff", staffPurpose, body, c.ip)
	switch {
	case errors.Is(err, auth.ErrPasskeyRequired):
		return nil, errStaffPasskey
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	ss, err := s.Staff.Start(ctx, u.ID, sess.ID, req.Body.Organizer, reason, c.ip, c.ua)
	switch {
	case errors.Is(err, staff.ErrAlertsOff):
		return nil, errAlertsOff
	case errors.Is(err, staff.ErrNoOrganizer):
		return nil, errNoOrganizer
	case errors.Is(err, staff.ErrNotAllowed):
		return nil, notFound() // revoked, or signed out, since staffCaller looked
	case err != nil:
		return nil, err
	}
	return StartStaffSession201JSONResponse(toStaffSession(ss)), nil
}

func (s *server) EndStaffSession(ctx context.Context, req EndStaffSessionRequestObject) (EndStaffSessionResponseObject, error) {
	u, sess, err := s.staffCaller(ctx)
	if err != nil {
		return nil, err
	}
	note := ""
	if req.Body != nil && req.Body.Note != nil {
		note = strings.TrimSpace(*req.Body.Note)
	}
	if utf8.RuneCountInString(note) > 2000 {
		return nil, invalidField("invalid_note", "note", "Keep the note to 2,000 characters.")
	}
	err = s.Staff.End(ctx, u.ID, sess.ID, "staff", note)
	if errors.Is(err, staff.ErrNoSession) {
		return nil, notFound()
	}
	if err != nil {
		return nil, err
	}
	return EndStaffSession204Response{}, nil
}

func (s *server) SearchStaffOrganizers(ctx context.Context, req SearchStaffOrganizersRequestObject) (SearchStaffOrganizersResponseObject, error) {
	u, _, err := s.staffCaller(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.TrimSpace(req.Params.Q)
	if utf8.RuneCountInString(req.Params.Q) > 120 || utf8.RuneCountInString(q) < 2 {
		return nil, invalidField("invalid_query", "q", "Type at least 2 characters of the organizer's name or web address, and no more than 120.")
	}
	list, err := s.Staff.Search(ctx, u.ID, q)
	if err != nil {
		return nil, err
	}
	out := make(SearchStaffOrganizers200JSONResponse, 0, len(list))
	for _, o := range list {
		out = append(out, StaffOrganizer{Name: o.Name, Slug: o.Slug})
	}
	return out, nil
}

func (s *server) ListStaffSessions(ctx context.Context, _ ListStaffSessionsRequestObject) (ListStaffSessionsResponseObject, error) {
	u, _, err := s.staffCaller(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.Staff.List(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return ListStaffSessions200JSONResponse(toStaffSessionLogs(list)), nil
}

func toStaffSessionLogs(list []staff.SessionLog) []StaffSessionLog {
	out := make([]StaffSessionLog, 0, len(list))
	for _, l := range list {
		e := StaffSessionLog{Id: l.ID, StaffName: l.StaffName, OrganizerSlug: l.OrganizerSlug, OrganizerName: l.OrganizerName, Reason: l.Reason,
			StartedAt: l.StartedAt, ExpiresAt: l.ExpiresAt, EndedAt: nullOf(l.EndedAt), EndNote: nullOf(l.EndNote),
			EndedHow: nullable.NewNullNullable[StaffSessionLogEndedHow](), Requests: make([]StaffRequest, 0, len(l.Requests))}
		if l.EndedHow != nil {
			e.EndedHow.Set(StaffSessionLogEndedHow(*l.EndedHow))
		}
		for _, r := range l.Requests {
			e.Requests = append(e.Requests, StaffRequest{Method: r.Method, Route: r.Route, At: r.At})
		}
		out = append(out, e)
	}
	return out
}

// ListStaffAccess shows an organizer's owners and admins every staff
// session on it, with names from app_staff_directory: never an email.
func (s *server) ListStaffAccess(ctx context.Context, req ListStaffAccessRequestObject) (ListStaffAccessResponseObject, error) {
	var list []staff.SessionLog
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, SessionOnly: true}, func(_ organizers.Organizer, tx pgx.Tx) (err error) {
		list, err = staff.ForOrganizer(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ListStaffAccess200JSONResponse(toStaffSessionLogs(list)), nil
}
