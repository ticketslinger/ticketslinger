package sales_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func newService(t *testing.T) *sales.Service {
	t.Helper()
	return &sales.Service{Pool: testdb.New(t), Fee: pricing.Fee{BP: 200, Cents: 50}}
}

type seedOpts struct {
	quota int
	price int64
	taxBP int
}

// fixture is organizer acme (with a member, for catalogue edits) and its live
// event gala, ending in a week, selling General out of quota Hall.
type fixture struct {
	owner            *pgxpool.Pool
	user, org, event uuid.UUID
	general, hall    uuid.UUID
}

func seed(t *testing.T, pool *pgxpool.Pool, o seedOpts) fixture {
	t.Helper()
	f := fixture{owner: testdb.Owner(t, pool)}
	f.must(t, f.owner.QueryRow(context.Background(), `insert into users(email, name) values ('a@example.com', 'A') returning id`).Scan(&f.user))
	f.org = f.organizer(t, "acme")
	f.exec(t, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'owner')`, f.org, f.user)
	f.event, f.general, f.hall = f.liveEvent(t, f.org, o)
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

func (f fixture) organizer(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	return f.id(t, `insert into organizers(name, slug) values (initcap($1), $1) returning id`, slug)
}

// liveEvent adds gala to org with General in quota Hall.
func (f fixture) liveEvent(t *testing.T, org uuid.UUID, o seedOpts) (event, general, hall uuid.UUID) {
	t.Helper()
	event = f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status, tax_rate_bp)
		values ($1, 'gala', 'Gala', 'America/New_York', now() + interval '6 days', now() + interval '7 days', 'live', $2) returning id`, org, o.taxBP)
	general = f.ticketType(t, org, event, "General", o.price, false)
	hall = f.quota(t, org, event, "Hall", o.quota, general)
	return event, general, hall
}

func (f fixture) ticketType(t *testing.T, org, event uuid.UUID, name string, price int64, hidden bool) uuid.UUID {
	t.Helper()
	return f.id(t, `insert into ticket_types(organizer_id, event_id, name, price_cents, hidden, position)
		values ($1, $2, $3, $4, $5, (select count(*) + 1 from ticket_types where event_id = $2)) returning id`, org, event, name, price, hidden)
}

func (f fixture) quota(t *testing.T, org, event uuid.UUID, name string, size int, tts ...uuid.UUID) uuid.UUID {
	t.Helper()
	q := f.id(t, `insert into quotas(organizer_id, event_id, name, size) values ($1, $2, $3, $4) returning id`, org, event, name, size)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) select $1, $2, $3, unnest($4::uuid[])`, org, event, q, tts)
	return q
}

func (f fixture) promo(t *testing.T, code, kind string, value int64) uuid.UUID {
	t.Helper()
	return f.id(t, `insert into promo_codes(organizer_id, event_id, code, kind, value) values ($1, $2, $3, $4, $5) returning id`, f.org, f.event, code, kind, value)
}

func invalidCode(err error) string {
	var i catalogue.ErrInvalid
	if errors.As(err, &i) {
		return i.Code
	}
	return ""
}

func remaining(err error) map[uuid.UUID]int {
	var so sales.SoldOut
	if !errors.As(err, &so) {
		return nil
	}
	m := map[uuid.UUID]int{}
	for _, a := range so.Available {
		m[a.TicketTypeID] = a.Remaining
	}
	return m
}

// waitForLockWaiters returns once n backends of this database wait on a lock.
func waitForLockWaiters(t *testing.T, owner *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := owner.QueryRow(context.Background(), `select count(distinct l.pid) from pg_locks l
			join pg_stat_activity a on a.pid = l.pid where not l.granted and a.datname = current_database()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waiting on a lock after 10s, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
