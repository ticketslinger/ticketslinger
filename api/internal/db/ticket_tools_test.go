package db_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

// ticketToolsFixture adds to orderToolsFixture: event A's types GA $25
// (quota 10), VIP $25 (quota 1) and Balcony $40, two GA tickets on the paid
// online order, a comp order with one GA ticket, a second event of A with
// a type of its own, and one ticket on B's order.
type ticketToolsFixture struct {
	orderToolsFixture
	ga, vip, balcony, otherEvent uuid.UUID // ticket types
	paid                         [2]uuid.UUID
	comp, compTicket, ticketB    uuid.UUID
}

func newTicketToolsFixture(t *testing.T) ticketToolsFixture {
	t.Helper()
	f := ticketToolsFixture{orderToolsFixture: newOrderToolsFixture(t)}
	ctx := context.Background()
	typ := func(org, event uuid.UUID, name string, price int64, quota int) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := f.owner.QueryRow(ctx, `insert into ticket_types(organizer_id, event_id, name, price_cents) values ($1, $2, $3, $4) returning id`,
			org, event, name, price).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if quota > 0 {
			if _, err := f.owner.Exec(ctx, `with q as (insert into quotas(organizer_id, event_id, name, size) values ($1, $2, $3, $4) returning id)
				insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) select $1, $2, id, $5 from q`,
				org, event, name, quota, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	f.ga, f.vip, f.balcony = typ(f.oa, f.eventA, "GA", 2500, 10), typ(f.oa, f.eventA, "VIP", 2500, 1), typ(f.oa, f.eventA, "Balcony", 4000, 0)
	var second uuid.UUID
	if err := f.owner.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'other', 'Other', 'UTC', now() + interval '2 days', now() + interval '49 hours', 'live') returning id`, f.oa).Scan(&second); err != nil {
		t.Fatal(err)
	}
	f.otherEvent = typ(f.oa, second, "GA", 2500, 0)
	var err error
	if f.comp, err = f.order(f.oa, f.eventA, "comp", nil, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	f.paid[0], f.paid[1] = f.ticket(t, f.oa, f.eventA, f.orderA, f.ga), f.ticket(t, f.oa, f.eventA, f.orderA, f.ga)
	f.compTicket = f.ticket(t, f.oa, f.eventA, f.comp, f.ga)
	f.ticketB = f.ticket(t, f.ob, f.eventB, f.orderB, typ(f.ob, f.eventB, "GA", 2500, 0))
	return f
}

func (f ticketToolsFixture) ticket(t *testing.T, org, event, order, typ uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents,
			discount_cents, tax_cents, fee_cents, charge_cents, secret)
		values ($1, $2, $3, $4, 2500, 0, 0, 100, 2500, uuid_send(gen_random_uuid())) returning id`, org, event, order, typ).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f ticketToolsFixture) set(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// appText runs a one-value query as ts_app for a member of org.
func (f ticketToolsFixture) appText(user, org uuid.UUID, q string, args ...any) (string, error) {
	var s string
	err := db.WithOrganizer(context.Background(), f.pool, user, org, func(tx pgx.Tx, _ string) error {
		return tx.QueryRow(context.Background(), q, args...).Scan(&s)
	})
	return s, err
}

func wantRefusal(t *testing.T, what string, err error, msg string) {
	t.Helper()
	if code, got := pgCode(err); code != "22023" || got != msg {
		t.Errorf("%s: %v, want 22023 %s", what, err, msg)
	}
}

func TestBlockColumnsOneWay(t *testing.T) {
	f := newTicketToolsFixture(t)
	ctx := context.Background()
	const block = `update tickets set blocked_at = statement_timestamp(), blocked_reason = $2 where id = $1`
	if n, err := f.asApp(f.admin, f.oa, block, f.paid[0], "Chargeback"); err != nil || n != 1 {
		t.Fatalf("block: %d %v", n, err)
	}
	_, err := f.asApp(f.ua, f.oa, `update tickets set blocked_at = statement_timestamp() - interval '1 hour' where id = $1`, f.paid[1])
	wantCode(t, "back-dated block", err, "42501")
	if n, err := f.asApp(f.ua, f.oa, `update tickets set blocked_at = null, blocked_reason = null where id = $1`, f.paid[0]); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	_, err = f.asApp(f.ua, f.oa, block, f.paid[1], strings.Repeat("x", 201))
	wantCode(t, "201-character reason", err, "23514")
	_, err = f.asApp(f.ua, f.oa, `update tickets set blocked_reason = 'x' where id = $1`, f.paid[1])
	wantCode(t, "reason with no block", err, "23514")

	// Owners and admins only, as people: a check-in member or a write key
	// can't block, even with the column grant.
	_, err = f.asApp(f.checkin, f.oa, block, f.paid[1], "")
	wantCode(t, "check-in member blocks", err, "42501")
	key := seedKey(t, f.pool, f.oa, "write", false)
	err = db.WithPrincipal(ctx, f.pool, db.Principal{KeyID: key, KeyOrg: f.oa, KeyScope: "write"}, f.oa, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, block, f.paid[1], "")
		return err
	})
	wantCode(t, "write key blocks", err, "42501")

	f.set(t, `update tickets set status = 'void' where id = $1`, f.compTicket)
	_, err = f.asApp(f.ua, f.oa, block, f.compTicket, "")
	wantCode(t, "block a void ticket", err, "42501")

	// Guards: no grant on either column, before this migration or after.
	_, err = f.asApp(f.ua, f.oa, `update tickets set secret = uuid_send(gen_random_uuid()) where id = $1`, f.paid[1])
	wantCode(t, "ts_app sets the secret", err, "42501")
	_, err = f.asApp(f.ua, f.oa, `update tickets set ticket_type_id = $2 where id = $1`, f.paid[1], f.vip)
	wantCode(t, "ts_app sets the type", err, "42501")
}

func TestCheckinsBlockedResult(t *testing.T) {
	f := newTicketToolsFixture(t)
	const q = `insert into checkins(organizer_id, event_id, ticket_id, method, result, actor_user_id) values ($1, $2, $3, 'manual', $4, $5)`
	if _, err := f.owner.Exec(context.Background(), q, f.oa, f.eventA, f.paid[0], "blocked", f.ua); err != nil {
		t.Fatalf("blocked: %v", err)
	}
	// Guard: passes before the migration too.
	_, err := f.owner.Exec(context.Background(), q, f.oa, f.eventA, f.paid[0], "other", f.ua)
	wantCode(t, "result other", err, "23514")
}

func TestSetTicketType(t *testing.T) {
	f := newTicketToolsFixture(t)
	const q = `select set_ticket_type($1, $2)`
	if _, err := f.asApp(f.ua, f.oa, q, f.paid[0], f.vip); err != nil {
		t.Fatalf("GA to VIP: %v", err)
	}
	var typ uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `select ticket_type_id from tickets where id = $1`, f.paid[0]).Scan(&typ); err != nil || typ != f.vip {
		t.Fatalf("type after the change: %v %v", typ, err)
	}
	_, err := f.asApp(f.ua, f.oa, q, f.paid[1], f.balcony)
	wantRefusal(t, "GA to Balcony", err, "price_differs")
	if _, err := f.asApp(f.admin, f.oa, q, f.compTicket, f.balcony); err != nil {
		t.Errorf("comp GA to Balcony: %v", err)
	}
	_, err = f.asApp(f.ua, f.oa, q, f.paid[0], f.vip)
	wantRefusal(t, "VIP to VIP", err, "same_type")
	_, err = f.asApp(f.ua, f.oa, q, f.paid[0], f.otherEvent)
	wantRefusal(t, "another event's type", err, "unknown_ticket_type")
	_, err = f.asApp(f.ua, f.oa, q, f.paid[0], uuid.New())
	wantRefusal(t, "no such type", err, "unknown_ticket_type")

	_, err = f.asApp(f.checkin, f.oa, q, f.paid[1], f.vip)
	wantCode(t, "check-in member", err, "42501")
	_, err = f.asApp(f.ua, f.oa, q, f.ticketB, f.vip)
	wantCode(t, "another organizer's ticket", err, "42501")

	f.set(t, `update tickets set checked_in_at = now() where id = $1`, f.paid[1])
	_, err = f.asApp(f.ua, f.oa, q, f.paid[1], f.vip)
	wantRefusal(t, "checked in", err, "checked_in")
	f.set(t, `update tickets set status = 'void' where id = $1`, f.paid[1])
	_, err = f.asApp(f.ua, f.oa, q, f.paid[1], f.vip)
	wantRefusal(t, "void", err, "ticket_void")
}

func TestSetTicketSecret(t *testing.T) {
	f := newTicketToolsFixture(t)
	const q = `select set_ticket_secret($1, $2)`
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	if _, err := f.asApp(f.ua, f.oa, q, f.paid[0], secret); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	var got []byte
	if err := f.owner.QueryRow(context.Background(), `select secret from tickets where id = $1`, f.paid[0]).Scan(&got); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("secret after: %x %v, want %x", got, err, secret)
	}
	_, err := f.asApp(f.ua, f.oa, q, f.paid[1], secret[:15])
	wantRefusal(t, "15 bytes", err, "bad_secret")
	_, err = f.asApp(f.checkin, f.oa, q, f.paid[1], make([]byte, 16))
	wantCode(t, "check-in member", err, "42501")
	_, err = f.asApp(f.ua, f.oa, q, f.ticketB, make([]byte, 16))
	wantCode(t, "another organizer's ticket", err, "42501")
	f.set(t, `update tickets set checked_in_at = now() where id = $1`, f.paid[1])
	_, err = f.asApp(f.ua, f.oa, q, f.paid[1], make([]byte, 16))
	wantRefusal(t, "checked in", err, "checked_in")
	f.set(t, `update tickets set status = 'void' where id = $1`, f.compTicket)
	_, err = f.asApp(f.ua, f.oa, q, f.compTicket, make([]byte, 16))
	wantRefusal(t, "void", err, "ticket_void")
}

func TestForceOrderEmail(t *testing.T) {
	f := newTicketToolsFixture(t)
	ctx := context.Background()
	jobs := func() (all, live int) {
		t.Helper()
		if err := f.owner.QueryRow(ctx, `select count(*), count(*) filter (where run_at <> 'infinity'
				and (locked_until is null or locked_until < now())) from jobs
			where kind = 'order_confirmation' and payload->>'order_id' = $1::text`, f.orderA).Scan(&all, &live); err != nil {
			t.Fatal(err)
		}
		return all, live
	}
	force := func(what, user string, org uuid.UUID, want string) {
		t.Helper()
		u := map[string]uuid.UUID{"owner": f.ua, "admin": f.admin}[user]
		if got, err := f.appText(u, org, `select force_order_email($1)`, f.orderA); err != nil || got != want {
			t.Errorf("%s: %q %v, want %s", what, got, err, want)
		}
	}

	f.set(t, `update orders set mail_status = 'sent', mail_requested_at = now() where id = $1`, f.orderA)
	if got, err := f.appText(f.ua, f.oa, `select queue_order_email($1)`, f.orderA); err != nil || got != "too_soon" {
		t.Fatalf("queue_order_email just after a send: %q %v", got, err)
	}
	force("force just after a send", "owner", f.oa, "queued")
	if all, _ := jobs(); all != 1 {
		t.Fatalf("jobs after force: %d", all)
	}
	force("queued with a job waiting", "admin", f.oa, "on_its_way")
	if all, _ := jobs(); all != 1 {
		t.Errorf("jobs after on_its_way: %d", all)
	}

	// A running job has read the old tickets already, so another follows.
	f.set(t, `update jobs set locked_until = now() + interval '5 minutes' where payload->>'order_id' = $1::text`, f.orderA)
	force("queued with the job running", "owner", f.oa, "queued")
	if all, live := jobs(); all != 2 || live != 1 {
		t.Errorf("jobs beside a running one: %d, %d waiting", all, live)
	}

	// A failing job is brought forward, not doubled.
	f.set(t, `delete from jobs`)
	f.set(t, `update orders set mail_status = 'failing', mail_attempts = 4 where id = $1`, f.orderA)
	f.set(t, `insert into jobs(kind, payload, run_at, attempts) values ('order_confirmation', jsonb_build_object('order_id', $1::uuid), now() + interval '1 hour', 9)`, f.orderA)
	force("failing with a job waiting", "owner", f.oa, "queued")
	if all, live := jobs(); all != 1 || live != 1 {
		t.Errorf("jobs after reviving: %d, %d waiting", all, live)
	}
	var due bool
	var attempts, mailAttempts int
	var status string
	if err := f.owner.QueryRow(ctx, `select j.run_at <= now(), j.attempts, o.mail_attempts, o.mail_status from jobs j, orders o
		where o.id = $1 and j.payload->>'order_id' = $1::text`, f.orderA).Scan(&due, &attempts, &mailAttempts, &status); err != nil ||
		!due || attempts != 0 || mailAttempts != 0 || status != "queued" {
		t.Errorf("after reviving: due %v, job attempts %d, mail attempts %d, status %s, %v", due, attempts, mailAttempts, status, err)
	}

	_, err := f.appText(f.checkin, f.oa, `select force_order_email($1)`, f.orderA)
	wantCode(t, "check-in member", err, "42501")
	_, err = f.appText(f.ua, f.oa, `select force_order_email($1)`, f.orderB)
	wantCode(t, "another organizer's order", err, "42501")

	// Blocked tickets have no QR to send.
	f.set(t, `delete from jobs`)
	f.set(t, `update orders set mail_status = null, mail_requested_at = null where id = $1`, f.orderA)
	f.set(t, `update tickets set blocked_at = now() where order_id = $1`, f.orderA)
	force("only blocked tickets", "owner", f.oa, "no_tickets")
	if got, err := f.appText(f.ua, f.oa, `select queue_order_email($1)`, f.orderA); err != nil || got != "no_tickets" {
		t.Errorf("queue_order_email, only blocked tickets: %q %v", got, err)
	}
}

func TestOrderByLinkBlocked(t *testing.T) {
	f := newTicketToolsFixture(t)
	f.set(t, `update tickets set blocked_at = now() where id = $1`, f.paid[0])
	var code string
	if err := f.owner.QueryRow(context.Background(), `select code from orders where id = $1`, f.orderA).Scan(&code); err != nil {
		t.Fatal(err)
	}
	raw, err := f.appText(f.ua, f.oa, `select tickets::text from order_by_link($1::text, sha256($1::text::bytea))`, code)
	if err != nil {
		t.Fatal(err)
	}
	var tickets []struct {
		ID      uuid.UUID `json:"id"`
		Blocked *bool     `json:"blocked"`
		Secret  *string   `json:"secret"`
	}
	if err := json.Unmarshal([]byte(raw), &tickets); err != nil || len(tickets) != 2 {
		t.Fatalf("tickets: %s %v", raw, err)
	}
	for _, k := range tickets {
		blocked := k.ID == f.paid[0]
		if k.Blocked == nil || *k.Blocked != blocked || (k.Secret == nil) != blocked {
			t.Errorf("ticket %s: blocked %v, secret %v; want blocked %v and a secret only when not", k.ID, k.Blocked, k.Secret, blocked)
		}
	}
}
