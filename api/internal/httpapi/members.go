package httpapi

import (
	"context"
	"errors"
	"net/mail"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

// The team is for people: keys never see or change it. Admins read it; only
// owners change it, and add_member/remove_member check that again in SQL.
var (
	teamRead  = Access{Roles: ownerAdmin, SessionOnly: true}
	teamWrite = Access{Roles: []string{"owner"}, Write: true, SessionOnly: true}

	errInvalidEmail  = invalidField("invalid_email", "email", "Enter the email address they sign in to TicketSlinger with.")
	errInvalidRole   = invalidField("invalid_role", "role", "Pick Check-in or Admin. Owners can't be added here.")
	errNoAccount     = &apiError{404, "no_account", "There's no TicketSlinger account with that email yet. Ask them to sign up at app.ticketslinger.com, then add them here."}
	errAlreadyMember = &apiError{409, "already_member", "They're already on your team."}
	errCannotRemove  = &apiError{409, "cannot_remove", "Owners can't be removed here, and you can't remove yourself."}
	errForbiddenRole = &apiError{403, "forbidden_role", "Your role here doesn't allow that."}
)

// member reads one of the scoped organizer's members. The directory hands
// rows only to a signed-in owner or admin, so that gate lives in one place.
const memberSelect = `select d.user_id, d.name, d.email::text, m.role from app_member_directory() d
	join memberships m on m.user_id = d.user_id and m.organizer_id = app_organizer()`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	var role string
	err := row.Scan(&m.UserId, &m.Name, &m.Email, &role)
	m.Role = MemberRole(role)
	return m, err
}

func (s *server) ListMembers(ctx context.Context, req ListMembersRequestObject) (ListMembersResponseObject, error) {
	out := ListMembers200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, teamRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, memberSelect+` order by m.created_at, d.user_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMember(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) AddMember(ctx context.Context, req AddMemberRequestObject) (AddMemberResponseObject, error) {
	var out Member
	err := s.inOrganizer(ctx, req.Organizer, teamWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		email, role := req.Body.Email, string(req.Body.Role)
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 254 {
			return errInvalidEmail
		}
		if role != "admin" && role != "checkin" {
			return errInvalidRole
		}
		var r string
		if err := tx.QueryRow(ctx, `select add_member($1, $2)`, email, role).Scan(&r); err != nil {
			return err
		}
		switch r {
		case "no_account":
			return errNoAccount
		case "already":
			return errAlreadyMember
		case "forbidden":
			return errForbiddenRole
		case "added":
		default:
			return errors.New("add_member: unexpected result " + r)
		}
		var err error
		if out, err = scanMember(tx.QueryRow(ctx, memberSelect+` where d.email = $1`, email)); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "member.added",
			EntityType: "member", EntityID: out.UserId, After: map[string]string{"email": out.Email, "role": role}})
	})
	if err != nil {
		return nil, err
	}
	return AddMember201JSONResponse(out), nil
}

type revokedKey struct {
	ID                  uuid.UUID
	Name, Prefix, Scope string
}

func (s *server) RemoveMember(ctx context.Context, req RemoveMemberRequestObject) (RemoveMemberResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, teamWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		// Read first: once removed, the directory no longer shows them.
		before, err := scanMember(tx.QueryRow(ctx, memberSelect+` where d.user_id = $1`, req.Member))
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		if err != nil {
			return err
		}
		// Their live keys, read before remove_member revokes them, for the audit log.
		rows, err := tx.Query(ctx, `select id, name, prefix, scope from api_keys
			where organizer_id = $1 and created_by = $2 and revoked_at is null`, org.ID, req.Member)
		if err != nil {
			return err
		}
		keys, err := pgx.CollectRows(rows, pgx.RowToStructByPos[revokedKey])
		if err != nil {
			return err
		}
		var r string
		if err := tx.QueryRow(ctx, `select remove_member($1)`, req.Member).Scan(&r); err != nil {
			return err
		}
		switch r {
		case "not_found":
			return notFound()
		case "cannot_remove":
			return errCannotRemove
		case "forbidden":
			return errForbiddenRole
		case "removed":
		default:
			return errors.New("remove_member: unexpected result " + r)
		}
		for _, k := range keys {
			if err := audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "api_key.revoked",
				EntityType: "api_key", EntityID: k.ID, Before: map[string]string{"name": k.Name, "prefix": k.Prefix, "scope": k.Scope}}); err != nil {
				return err
			}
		}
		return audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "member.removed",
			EntityType: "member", EntityID: uuid.UUID(req.Member), Before: map[string]string{"email": before.Email, "role": string(before.Role)}})
	})
	if err != nil {
		return nil, err
	}
	return RemoveMember204Response{}, nil
}
