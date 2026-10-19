package checkin_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/checkin"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// fixture is organizer acme, its owner, and its event gala selling General.
// Orders and tickets are added by each test through the owner connection.
type fixture struct {
	app, owner     *pgxpool.Pool
	user, org      uuid.UUID
	event, general uuid.UUID
}

// ticket is a seeded ticket and the QR text that carries its secret.
type ticket struct {
	id uuid.UUID
	qr string
}

func seed(t *testing.T) fixture {
	t.Helper()
	f := fixture{app: testdb.New(t)}
	f.owner = testdb.Owner(t, f.app)
	f.user = f.id(t, `insert into users(email, name) values ('a@example.com', 'A') returning id`)
	f.org = f.organizer(t, "acme")
	f.exec(t, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'owner')`, f.org, f.user)
	f.event, f.general = f.liveEvent(t, f.org, "gala")
	return f
}

func (f fixture) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.owner.Exec(context.Background(), sql, args...)
	f.must(t, err)
}

func (f fixture) id(t *testing.T, sql string, args ...any) (id uuid.UUID) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), sql, args...).Scan(&id))
	return id
}

func (f fixture) count(t *testing.T, sql string, args ...any) (n int) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (f fixture) organizer(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	return f.id(t, `insert into organizers(name, slug) values (initcap($1), $1) returning id`, slug)
}

// liveEvent adds an event to org with one ticket type, General.
func (f fixture) liveEvent(t *testing.T, org uuid.UUID, slug string) (event, general uuid.UUID) {
	t.Helper()
	event = f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, $2, initcap($2), 'America/New_York', now() + interval '6 days', now() + interval '7 days', 'live') returning id`, org, slug)
	general = f.id(t, `insert into ticket_types(organizer_id, event_id, name, price_cents, position)
		values ($1, $2, 'General', 2500, 1) returning id`, org, event)
	return event, general
}

// order adds a paid order to event with one ticket per attendee name, each
// with a fresh 16-byte secret.
func (f fixture) order(t *testing.T, org, event, ticketType uuid.UUID, code, email string, names ...string) []ticket {
	t.Helper()
	o := f.id(t, `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details)
		values ($1, $2, $3, $4, 'paid', 0, 0, 0, 0, 0, 'absorb', '{}', '{}') returning id`, org, event, code, email)
	out := make([]ticket, len(names))
	for i, name := range names {
		secret := make([]byte, 16)
		_, _ = rand.Read(secret)
		out[i] = ticket{qr: base64.RawURLEncoding.EncodeToString(secret)}
		out[i].id = f.id(t, `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, attendee_name, secret)
			values ($1, $2, $3, $4, 0, 0, 0, 0, 0, $5, $6) returning id`, org, event, o, ticketType, name, secret)
	}
	return out
}

// checkIn runs one check-in at event as the owner, in an organizer transaction.
func (f fixture) checkIn(t *testing.T, event uuid.UUID, in checkin.Input) checkin.Result {
	t.Helper()
	r, err := f.tryCheckIn(context.Background(), event, in)
	f.must(t, err)
	return r
}

func (f fixture) tryCheckIn(ctx context.Context, event uuid.UUID, in checkin.Input) (r checkin.Result, err error) {
	in.Actor = audit.Actor{User: f.user}
	err = db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		r, err = checkin.CheckIn(ctx, tx, f.org, event, in)
		return err
	})
	return r, err
}

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func actorOf(f fixture) audit.Actor { return audit.Actor{User: f.user} }
