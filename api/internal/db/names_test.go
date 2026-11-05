package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// namesFixture is the staff fixture plus, on A, a live event starting
// tomorrow and a paid order (TS-NNNNN, link token "link-n") with two tickets.
type namesFixture struct {
	staffFixture
	event, order uuid.UUID
	tickets      [2]uuid.UUID
	hash         []byte
}

func newNamesFixture(t *testing.T) namesFixture {
	t.Helper()
	f := namesFixture{staffFixture: newStaffFixture(t)}
	ctx := context.Background()
	if err := f.owner.QueryRow(ctx, `with
		e as (insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
			values ($1, 'names-show', 'Names', 'UTC', now() + interval '1 day', now() + interval '25 hours', 'live') returning id),
		tt as (insert into ticket_types(organizer_id, event_id, name, price_cents, position) select $1, id, 'General', 2500, 1 from e returning id, event_id)
		insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents,
				fee_mode, quote, details, magic_token_hash)
			select $1, id, 'TS-NNNNN', 'n@example.com', 'paid', 5000, 0, 0, 200, 5200, 'pass', '{}', '{}', sha256('link-n') from e
		returning id, event_id, magic_token_hash`, f.oa).Scan(&f.order, &f.event, &f.hash); err != nil {
		t.Fatal(err)
	}
	rows, err := f.owner.Query(ctx, `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents,
			tax_cents, fee_cents, charge_cents, attendee_name, secret)
		select $1, $2, $3, tt.id, 2500, 0, 0, 100, 2500, 'Ada', decode(md5(random()::text), 'hex')
		  from ticket_types tt cross join generate_series(1, 2) where tt.event_id = $2 returning id`, f.oa, f.event, f.order)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) != 2 {
		t.Fatalf("tickets: %v %v", ids, err)
	}
	copy(f.tickets[:], ids)
	return f
}

// rename calls rename_ticket_by_link as the storefront does.
func (f namesFixture) rename(code string, hash []byte, ticket uuid.UUID, name string) (*string, error) {
	ctx := context.Background()
	var res *string
	err := system.Buyer(ctx, f.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select rename_ticket_by_link($1, $2, $3, $4)`, code, hash, ticket, name).Scan(&res)
	})
	return res, err
}

func (f namesFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f namesFixture) ticket(t *testing.T, id uuid.UUID) (name string, changes int) {
	t.Helper()
	if err := f.owner.QueryRow(context.Background(), `select attendee_name, name_changes from tickets where id = $1`, id).Scan(&name, &changes); err != nil {
		t.Fatal(err)
	}
	return name, changes
}

func (f namesFixture) renames(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), `select count(*) from audit_log where action = 'ticket.renamed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec test 7: the buyer's rename, every answer the function gives.
func TestRenameByLink(t *testing.T) {
	f := newNamesFixture(t)
	ctx := context.Background()
	tk := f.tickets[0]
	want := func(t *testing.T, name, answer string) {
		t.Helper()
		res, err := f.rename("ts-nnnnn", f.hash, tk, name)
		if err != nil || str(res) != answer {
			t.Fatalf("rename to %q: %s %v, want %s", name, str(res), err, answer)
		}
	}

	t.Run("renamed", func(t *testing.T) {
		want(t, "Grace Hopper", "renamed")
		if name, n := f.ticket(t, tk); name != "Grace Hopper" || n != 1 {
			t.Fatalf("ticket: %q %d, want Grace Hopper 1", name, n)
		}
		var org uuid.UUID
		var bySystem bool
		var before, after string
		if err := f.owner.QueryRow(ctx, `select organizer_id, actor_system, before::text, after::text from audit_log
			where action = 'ticket.renamed' and entity_type = 'ticket' and entity_id = $1 and actor_user_id is null`, tk).Scan(&org, &bySystem, &before, &after); err != nil {
			t.Fatal(err)
		}
		if org != f.oa || !bySystem || before != `{"attendee_name": "Ada"}` || after != `{"via": "order_link", "attendee_name": "Grace Hopper"}` {
			t.Fatalf("audit: %v %v %s %s", org, bySystem, before, after)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		want(t, "Grace Hopper", "unchanged")
		if _, n := f.ticket(t, tk); n != 1 || f.renames(t) != 1 {
			t.Fatalf("name_changes %d, audit rows %d, want 1 and 1", n, f.renames(t))
		}
	})
	t.Run("not found", func(t *testing.T) {
		var other uuid.UUID // A's other order (TS-AAAAA)
		if err := f.owner.QueryRow(ctx, `select t.id from tickets t join orders o on o.id = t.order_id where o.code = 'TS-AAAAA'`).Scan(&other); err != nil {
			t.Fatal(err)
		}
		for name, call := range map[string][3]any{
			"wrong hash":           {"TS-NNNNN", []byte("nope"), tk},
			"unknown code":         {"TS-ZZZZZ", f.hash, tk},
			"another order ticket": {"TS-NNNNN", f.hash, other},
		} {
			res, err := f.rename(call[0].(string), call[1].([]byte), call[2].(uuid.UUID), "Mallory")
			if err != nil || res != nil {
				t.Errorf("%s: %s %v, want NULL", name, str(res), err)
			}
		}
		if f.renames(t) != 1 {
			t.Fatalf("audit rows %d, want 1", f.renames(t))
		}
	})
	t.Run("locked", func(t *testing.T) {
		for name, q := range map[string][2]string{
			"names locked":     {`update events set names_locked_at = now() - interval '1 minute' where id = $1`, `update events set names_locked_at = null where id = $1`},
			"event started":    {`update events set starts_at = now() - interval '1 minute' where id = $1`, `update events set starts_at = now() + interval '1 day' where id = $1`},
			"event cancelled":  {`update events set status = 'cancelled' where id = $1`, `update events set status = 'live' where id = $1`},
			"ticket void":      {`update tickets set status = 'void' where event_id = $1`, `update tickets set status = 'valid' where event_id = $1`},
			"order refunded":   {`update orders set status = 'refunded' where event_id = $1`, `update orders set status = 'paid' where event_id = $1`},
			"order pending":    {`update orders set status = 'pending' where event_id = $1`, `update orders set status = 'paid' where event_id = $1`},
			"event ended":      {`update events set status = 'ended' where id = $1`, `update events set status = 'live' where id = $1`},
			"partially kept":   {`update orders set status = 'partially_refunded' where event_id = $1`, `update orders set status = 'paid' where event_id = $1`},
			"later lock is ok": {`update events set names_locked_at = now() + interval '2 days' where id = $1`, `update events set names_locked_at = null where id = $1`},
		} {
			f.exec(t, q[0], f.event)
			res, err := f.rename("TS-NNNNN", f.hash, tk, "Mallory")
			answer := "locked"
			if name == "partially kept" || name == "later lock is ok" {
				answer = "renamed"
			}
			if err != nil || str(res) != answer {
				t.Errorf("%s: %s %v, want %s", name, str(res), err, answer)
			}
			if answer == "renamed" {
				if res, err := f.rename("TS-NNNNN", f.hash, tk, "Grace Hopper"); err != nil || str(res) != "renamed" {
					t.Fatalf("%s: rename back: %s %v", name, str(res), err)
				}
			}
			f.exec(t, q[1], f.event)
		}
		f.exec(t, `update tickets set name_changes = 1 where id = $1`, tk)
		f.act(t, "suspend", uuid.NullUUID{})
		if res, err := f.rename("TS-NNNNN", f.hash, tk, "Mallory"); err != nil || str(res) != "locked" {
			t.Errorf("organizer suspended: %s %v, want locked", str(res), err)
		}
		f.act(t, "lift", uuid.NullUUID{})
		if name, n := f.ticket(t, tk); name != "Grace Hopper" || n != 1 {
			t.Fatalf("after refusals: %q %d, want Grace Hopper 1", name, n)
		}
	})
	t.Run("limit", func(t *testing.T) {
		other := f.tickets[1]
		for i := range 5 {
			if res, err := f.rename("TS-NNNNN", f.hash, other, "Name "+string(rune('A'+i))); err != nil || str(res) != "renamed" {
				t.Fatalf("rename %d: %s %v", i+1, str(res), err)
			}
		}
		if res, err := f.rename("TS-NNNNN", f.hash, other, "Sixth"); err != nil || str(res) != "limit" {
			t.Fatalf("sixth: %s %v, want limit", str(res), err)
		}
		if name, n := f.ticket(t, other); name != "Name E" || n != 5 {
			t.Fatalf("after limit: %q %d, want Name E 5", name, n)
		}
	})
	t.Run("bad name", func(t *testing.T) {
		// U+2028 and U+2029 pass [[:cntrl:]] under the en_US.utf8 collation, as
		// do all format characters: cleanName in the API is what refuses them.
		for _, name := range []string{"Ada\n", " Ada", "Ada ", "A\u0007da", "Ada\u0085", strings.Repeat("a", 121)} {
			res, err := f.rename("TS-NNNNN", f.hash, tk, name)
			if code, msg := pgCode(err); code != "22023" || msg != "bad_attendee" {
				t.Errorf("%q: %s %v, want 22023 bad_attendee", name, str(res), err)
			}
		}
		if res, err := f.rename("TS-NNNNN", f.hash, tk, strings.Repeat("a", 120)); err != nil || str(res) != "renamed" {
			t.Errorf("120 characters: %s %v, want renamed", str(res), err)
		}
		if res, err := f.rename("TS-NNNNN", f.hash, tk, ""); err != nil || str(res) != "renamed" {
			t.Errorf("empty: %s %v, want renamed", str(res), err)
		}
	})
}

// The organizer's rename: ts_app may write attendee_name, under tickets_scope,
// and nothing else the rename touches.
func TestAppCanOnlyUpdateAttendeeName(t *testing.T) {
	f := newNamesFixture(t)
	ctx := context.Background()
	run := func(q string, args ...any) (int64, error) {
		var n int64
		err := db.WithOrganizer(ctx, f.pool, f.ua, f.oa, func(tx pgx.Tx, _ string) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	if n, err := run(`update tickets set attendee_name = 'Grace' where id = $1`, f.tickets[0]); err != nil || n != 1 {
		t.Fatalf("attendee_name: %d %v, want 1", n, err)
	}
	for _, q := range []string{
		`update tickets set name_changes = 0 where id = $1`,
		`update tickets set secret = '\x00' where id = $1`,
		`update tickets set order_id = order_id where id = $1`,
	} {
		if _, err := run(q, f.tickets[0]); func() string { c, _ := pgCode(err); return c }() != "42501" {
			t.Errorf("%s: %v, want 42501", q, err)
		}
	}
	if n, err := run(`update tickets set attendee_name = 'Mallory' where organizer_id = $1`, f.ob); err != nil || n != 0 {
		t.Fatalf("B's tickets: %d %v, want 0", n, err)
	}
}

// The storefront role renames only through rename_ticket_by_link.
func TestPublicCanOnlyRenameThroughTheFunction(t *testing.T) {
	f := newNamesFixture(t)
	ctx := context.Background()
	err := system.Buyer(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update tickets set attendee_name = 'Mallory'`)
		return err
	})
	if code, _ := pgCode(err); code != "42501" {
		t.Fatalf("ts_public update tickets: %v, want 42501", err)
	}
}

// order_by_link says until when the buyer may rename, and how many changes
// each ticket has used.
func TestOrderByLinkNamesUntil(t *testing.T) {
	f := newNamesFixture(t)
	ctx := context.Background()
	var startsAt time.Time
	if err := f.owner.QueryRow(ctx, `select starts_at from events where id = $1`, f.event).Scan(&startsAt); err != nil {
		t.Fatal(err)
	}
	until := func() (*time.Time, []byte) {
		t.Helper()
		var at *time.Time
		var tickets []byte
		if err := f.pool.QueryRow(ctx, `select names_until, tickets from order_by_link('TS-NNNNN', $1)`, f.hash).Scan(&at, &tickets); err != nil {
			t.Fatal(err)
		}
		return at, tickets
	}
	at, tickets := until()
	if at == nil || !at.Equal(startsAt) {
		t.Errorf("live event: %v, want %v", at, startsAt)
	}
	var rows []map[string]any
	if err := json.Unmarshal(tickets, &rows); err != nil || len(rows) != 2 || rows[0]["name_changes"] != float64(0) {
		t.Errorf("tickets: %s %v", tickets, err)
	}
	lock := startsAt.Add(-time.Hour)
	f.exec(t, `update events set names_locked_at = $2 where id = $1`, f.event, lock)
	if at, _ := until(); at == nil || !at.Equal(lock) {
		t.Errorf("earlier lock: %v, want %v", at, lock)
	}
	f.exec(t, `update events set names_locked_at = now() - interval '1 minute' where id = $1`, f.event)
	if at, _ := until(); at != nil {
		t.Errorf("past lock: %v, want NULL", at)
	}
}
