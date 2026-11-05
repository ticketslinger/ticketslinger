package sales

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// CleanName is an attendee name as it's kept: trimmed, at most 120
// characters, valid UTF-8, and free of control and format characters (which
// take in the bidi overrides), private-use and surrogate code points, and
// line and paragraph separators, so a name can't break a line on the scanner
// or in a CSV. The one exception is a zero-width joiner or non-joiner between
// two other characters, which Persian and Indic names and emoji sequences
// need. Empty is fine: no name. errAttendees otherwise; the caller
// names the field. This is the rule; rename_ticket_by_link's check is only
// a backstop, since Postgres has no class for Cf.
func CleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 120 {
		return "", errAttendees
	}
	prev := rune(-1)
	for i, r := range s {
		if joiner(r) {
			if prev < 0 || joiner(prev) || i+utf8.RuneLen(r) == len(s) {
				return "", errAttendees
			}
		} else if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp) {
			return "", errAttendees
		}
		prev = r
	}
	return s, nil
}

// joiner is U+200C ZERO WIDTH NON-JOINER or U+200D ZERO WIDTH JOINER.
func joiner(r rune) bool { return r == '\u200c' || r == '\u200d' }

// RenameByLink is the buyer changing one ticket's name, with the token from
// the order's link, under rename_ticket_by_link's rules. An unknown code, a
// wrong token and a ticket of another order are all ErrNotFound, from one
// statement either way. The same name again is nil and writes nothing.
func (s *Service) RenameByLink(ctx context.Context, code, token string, ticket uuid.UUID, name string) error {
	hash, ok := linkHash(code, token)
	if !ok {
		return catalogue.ErrNotFound
	}
	name, err := CleanName(name)
	if err != nil {
		return errAttendeeName
	}
	var res *string
	err = system.Buyer(ctx, s.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select rename_ticket_by_link($1, $2, $3, $4)`, code, hash, ticket, name).Scan(&res)
	})
	if err != nil {
		return renameError(err)
	}
	switch {
	case res == nil:
		return catalogue.ErrNotFound
	case *res == "locked":
		return ErrNamesLocked
	case *res == "limit":
		return ErrNameLimit
	case *res == "renamed", *res == "unchanged":
		return nil
	}
	return fmt.Errorf("rename_ticket_by_link: unexpected answer %q", *res)
}

// renameError is the 422 for a name the database refused after Go let it
// through: the function's own check (22023 bad_attendee), or a NUL, which
// Postgres refuses in any text (22021). Anything else passes through.
func renameError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "22023" && pg.Message == "bad_attendee" || pg.Code == "22021") {
		return errAttendeeName
	}
	return err
}

// RenameTicket is the organizer changing a valid ticket's name, as often as
// they like and at any time, in the caller's organizer transaction. The
// ticket keeps its secret: a rename corrects a ticket, it doesn't hand it on.
// A void ticket is ErrTicketVoid; one that isn't this order's, ErrNotFound.
// The same name again is nil and writes no audit row.
func RenameTicket(ctx context.Context, tx pgx.Tx, org, event, order, ticket uuid.UUID, name string, actor audit.Actor) error {
	name, err := CleanName(name)
	if err != nil {
		return errAttendeeName
	}
	var old string
	err = tx.QueryRow(ctx, `update tickets set attendee_name = $1 where id = $2 and order_id = $3 and event_id = $4 and status = 'valid'
		returning old.attendee_name`, name, ticket, order, event).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		err := tx.QueryRow(ctx, `select status from tickets where id = $1 and order_id = $2 and event_id = $3`, ticket, order, event).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return catalogue.ErrNotFound
		case err != nil:
			return err
		}
		return ErrTicketVoid
	}
	if err != nil {
		return renameError(err)
	}
	if old == name {
		return nil
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "ticket.renamed", EntityType: "ticket", EntityID: ticket,
		Before: map[string]string{"attendee_name": old}, After: map[string]string{"attendee_name": name}})
}
