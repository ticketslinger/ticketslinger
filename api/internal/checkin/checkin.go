// Package checkin lets guests in at the door: one guarded UPDATE per scan
// decides, so two doors scanning one ticket at once let it in once.
package checkin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

// Result is what a check-in decided: accepted, duplicate, invalid or void,
// the ticket it found (nil for invalid; a duplicate's first check-in time is
// Ticket.CheckedInAt), and the event's count of valid tickets in and sold.
type Result struct {
	Result          string
	Ticket          *Ticket
	CheckedIn, Sold int
}

type Ticket struct {
	ID                                              uuid.UUID
	TicketTypeName, AttendeeName, OrderCode, Status string
	CheckedInAt                                     *time.Time
}

// Input is one check-in: a scanned QR text or a ticket picked by hand
// (exactly one; the HTTP layer enforces it), the gate, and who did it.
type Input struct {
	QR       string
	TicketID uuid.UUID
	Gate     string
	Actor    audit.Actor
}

// CheckIn decides one check-in at event in the caller's organizer
// transaction and records it in checkins. The scanned text is a ticket's
// credential: only its sha256 is stored, and it is never in an error.
func CheckIn(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, in Input) (Result, error) {
	if (in.Actor.User == uuid.Nil) == (in.Actor.Key == uuid.Nil) {
		return Result{}, errors.New("checkin: an actor is exactly one of a user or a key")
	}
	method, where, match := "manual", "id = $2", any(in.TicketID)
	var hash []byte
	if in.TicketID == uuid.Nil {
		method, where = "scan", "secret = $2"
		h := sha256.Sum256([]byte(in.QR))
		hash = h[:]
		secret, ok := decodeQR(in.QR)
		if !ok {
			return record(ctx, tx, org, event, in, method, hash, "invalid", nil)
		}
		match = secret
	}

	// Under READ COMMITTED a second, concurrent UPDATE of the same ticket
	// waits on the first's row lock, re-checks checked_in_at IS NULL and
	// matches nothing; the follow-up read then sees the committed time.
	var id uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE tickets SET checked_in_at = statement_timestamp()
		WHERE event_id = $1 AND `+where+` AND status = 'valid' AND checked_in_at IS NULL RETURNING id`, event, match).Scan(&id)
	if err == nil {
		return record(ctx, tx, org, event, in, method, hash, "accepted", &id)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT id, status FROM tickets WHERE event_id = $1 AND `+where, event, match).Scan(&id, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return record(ctx, tx, org, event, in, method, hash, "invalid", nil)
	case err != nil:
		return Result{}, err
	case status == "void":
		return record(ctx, tx, org, event, in, method, hash, "void", &id)
	}
	return record(ctx, tx, org, event, in, method, hash, "duplicate", &id)
}

// record writes the checkins row and reads back the ticket and the counts.
func record(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, in Input, method string, hash []byte, result string, ticket *uuid.UUID) (Result, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO checkins(organizer_id, event_id, ticket_id, value_hash, method, gate, result, actor_user_id, actor_api_key_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9)`,
		org, event, ticket, hash, method, in.Gate, result, nullable(in.Actor.User), nullable(in.Actor.Key)); err != nil {
		return Result{}, err
	}
	r := Result{Result: result}
	if ticket != nil {
		rows, _ := tx.Query(ctx, view+` WHERE t.id = $1`, *ticket)
		t, err := pgx.CollectExactlyOneRow(rows, scanTicket)
		if err != nil {
			return Result{}, err
		}
		r.Ticket = &t
	}
	var err error
	r.CheckedIn, r.Sold, err = Summary(ctx, tx, event)
	return r, err
}

const view = `SELECT t.id, tt.name, t.attendee_name, o.code, t.status, t.checked_in_at
	FROM tickets t JOIN ticket_types tt ON tt.id = t.ticket_type_id JOIN orders o ON o.id = t.order_id`

func scanTicket(row pgx.CollectableRow) (t Ticket, err error) {
	err = row.Scan(&t.ID, &t.TicketTypeName, &t.AttendeeName, &t.OrderCode, &t.Status, &t.CheckedInAt)
	return t, err
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Search finds up to 20 of event's tickets by attendee name or buyer email
// (substring, any case) or by order code (with or without "TS-"), valid ones
// first, then by name.
func Search(ctx context.Context, tx pgx.Tx, event uuid.UUID, q string) ([]Ticket, error) {
	q = strings.TrimSpace(q)
	code := strings.ToUpper(q)
	if !strings.HasPrefix(code, "TS-") {
		code = "TS-" + code
	}
	rows, _ := tx.Query(ctx, view+` WHERE t.event_id = $1
		AND (t.attendee_name ILIKE '%' || $2 || '%' OR o.email ILIKE '%' || $2 || '%' OR o.code = $3)
		ORDER BY t.status = 'valid' DESC, t.attendee_name, t.id LIMIT 20`, event, likeEscaper.Replace(q), code)
	return pgx.CollectRows(rows, scanTicket)
}

// Summary counts event's valid tickets: checked in, and sold.
func Summary(ctx context.Context, tx pgx.Tx, event uuid.UUID) (checkedIn, sold int, err error) {
	err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE checked_in_at IS NOT NULL), count(*)
		FROM tickets WHERE event_id = $1 AND status = 'valid'`, event).Scan(&checkedIn, &sold)
	return checkedIn, sold, err
}

// decodeQR reads a ticket QR: 22 base64url characters, no padding, in
// canonical form, carrying the ticket's 16-byte secret.
func decodeQR(s string) ([]byte, bool) {
	if len(s) != 22 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, err == nil && len(b) == 16
}

func nullable(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
