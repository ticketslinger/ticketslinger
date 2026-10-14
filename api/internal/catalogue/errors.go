// Package catalogue holds what an organizer sells: events, and in later
// tasks their tickets, quotas, promo codes and questions.
package catalogue

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound means no such row in the scoped organizer. Another tenant's
// row looks exactly the same.
var ErrNotFound = errors.New("catalogue: not found")

// ErrInvalid is input the rules refuse (HTTP 422). Message is for people.
type ErrInvalid struct{ Code, Message string }

func (e ErrInvalid) Error() string { return e.Code }

// ErrConflict is a request that clashes with what exists (HTTP 409).
type ErrConflict struct{ Code, Message string }

func (e ErrConflict) Error() string { return e.Code }

// dbError turns the constraint violations the Go rules should have caught
// first into the same errors they would have raised: 23514 is a failed CHECK,
// 23505 a taken slug.
func dbError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{"invalid_event", "Something in that event isn't allowed. Check the lengths, dates and tax rate."}
		case "23505":
			return ErrConflict{"slug_taken", "You already have an event at that web address. Pick another."}
		}
	}
	return err
}
