package db_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// orderToolsFixture is acme (A) with an owner, an admin and a check-in
// member, a live event and one paid online order; and organizer B with its
// owner, an event and an order of its own.
type orderToolsFixture struct {
	pool, owner        *pgxpool.Pool
	oa, ob             uuid.UUID // organizers A and B
	ua, admin, checkin uuid.UUID // A's members
	ub                 uuid.UUID // B's owner
	eventA, eventB     uuid.UUID
	orderA, orderB     uuid.UUID // paid online orders
	codes              *int
}

func newOrderToolsFixture(t *testing.T) orderToolsFixture {
	t.Helper()
	f := orderToolsFixture{pool: testdb.New(t), codes: new(int)}
	f.owner = testdb.Owner(t, f.pool)
	f.ua, f.oa = seedOrg(t, f.pool, "a@example.com", "acme")
	f.ub, f.ob = seedOrg(t, f.pool, "b@example.com", "org-b")
	f.admin = f.member(t, f.oa, "admin@example.com", "admin")
	f.checkin = f.member(t, f.oa, "door@example.com", "checkin")
	f.eventA, f.eventB = f.event(t, f.oa), f.event(t, f.ob)
	var err error
	if f.orderA, err = f.order(f.oa, f.eventA, "online", nil, 5200, 200, 0); err != nil {
		t.Fatal(err)
	}
	if f.orderB, err = f.order(f.ob, f.eventB, "online", nil, 5200, 200, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f orderToolsFixture) member(t *testing.T, org uuid.UUID, email, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `with u as (insert into users(email, name) values ($1, 'M') returning id)
		insert into memberships(organizer_id, user_id, role) select $2, id, $3 from u returning user_id`, email, org, role).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f orderToolsFixture) event(t *testing.T, org uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'show', 'Show', 'UTC', now() + interval '1 day', now() + interval '25 hours', 'live') returning id`, org).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// order inserts a paid order as the schema owner, so only CHECKs refuse it.
func (f orderToolsFixture) order(org, event uuid.UUID, source string, method *string, total, fee, tax int64) (uuid.UUID, error) {
	*f.codes++
	var id uuid.UUID
	code := fmt.Sprintf("TS-%05d", *f.codes)
	err := f.owner.QueryRow(context.Background(), `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents,
			discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details, magic_token_hash, source, manual_method)
		values ($1, $2, $3, 'buyer@example.com', 'paid', $4::bigint - $5::bigint - $6::bigint, 0, $6, $5, $4, 'pass', '{}', '{}', sha256($3::text::bytea), $7, $8)
		returning id`, org, event, code, total, fee, tax, source, method).Scan(&id)
	return id, err
}

// asApp runs q as ts_app for a member of org and returns the rows it touched.
func (f orderToolsFixture) asApp(user, org uuid.UUID, q string, args ...any) (int64, error) {
	var n int64
	err := db.WithOrganizer(context.Background(), f.pool, user, org, func(tx pgx.Tx, _ string) error {
		tag, err := tx.Exec(context.Background(), q, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

// asSystem runs q as ts_system scoped to org and returns the rows it touched.
func (f orderToolsFixture) asSystem(org uuid.UUID, q string, args ...any) (int64, error) {
	var n int64
	err := system.Run(context.Background(), f.pool, org, func(tx pgx.Tx) error {
		tag, err := tx.Exec(context.Background(), q, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got, msg := pgCode(err); got != code {
		t.Errorf("%s: %v (%s %s), want %s", what, err, got, msg, code)
	}
}

func ptr(s string) *string { return &s }

func TestOrderSourceRules(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		what, source string
		method       *string
		total, fee   int64
		tax          int64
	}{
		{"manual without a method", "manual", nil, 1000, 0, 0},
		{"online with a method", "online", ptr("cash"), 1000, 0, 0},
		{"comp with a total", "comp", nil, 100, 0, 0},
		{"manual with a fee", "manual", ptr("cash"), 1000, 50, 0},
		{"manual with tax", "manual", ptr("cash"), 1000, 0, 1},
		{"unknown method", "manual", ptr("barter"), 1000, 0, 0},
	} {
		_, err := f.order(f.oa, f.eventA, c.source, c.method, c.total, c.fee, c.tax)
		wantCode(t, c.what, err, "23514")
	}
	manual, err := f.order(f.oa, f.eventA, "manual", ptr("invoice"), 1001, 0, 0)
	if err != nil {
		t.Fatalf("manual invoice: %v", err)
	}
	if _, err := f.order(f.oa, f.eventA, "comp", nil, 0, 0, 0); err != nil {
		t.Fatalf("comp at 0: %v", err)
	}
	if _, err := f.owner.Exec(ctx, `update orders set buyer_name = $1 where id = $2`, strings.Repeat("x", 121), manual); err == nil {
		t.Error("a 121-character buyer name was stored")
	}

	// Nothing changes how an order came to be, the system role included.
	for _, set := range []string{`source = 'comp', total_cents = 0, fee_cents = 0, subtotal_cents = 0`,
		`buyer_name = 'x'`, `manual_method = 'cash'`, `created_by = '` + f.ua.String() + `'`} {
		_, err := f.asSystem(f.oa, `update orders set `+set+` where id = $1`, manual)
		wantCode(t, "ts_system set "+set, err, "42501")
	}
	if n, err := f.asSystem(f.oa, `update orders set status = 'cancelled' where id = $1`, manual); err != nil || n != 1 {
		t.Fatalf("ts_system set status: %d %v", n, err)
	}
	if _, err := f.owner.Exec(ctx, `update orders set source = 'manual', manual_method = 'cash' where id = $1`, f.orderA); err == nil {
		t.Error("the schema owner turned an online order manual")
	}

	// Deleting the member who made an order still works: the creator
	// goes to NULL, the one change to created_by the trigger lets through.
	maker := f.member(t, f.oa, "maker@example.com", "admin")
	made, err := f.order(f.oa, f.eventA, "comp", nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `update orders set created_by = $1 where id = $2`, maker, made); err == nil {
		t.Error("created_by was set after creation")
	}
	if _, err := f.owner.Exec(ctx, `alter table orders disable trigger orders_source_fixed;
		update orders set created_by = '`+maker.String()+`' where id = '`+made.String()+`';
		alter table orders enable trigger orders_source_fixed`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `delete from users where id = $1`, maker); err != nil {
		t.Fatalf("delete the order's creator: %v", err)
	}
	var by *uuid.UUID
	if err := f.owner.QueryRow(ctx, `select created_by from orders where id = $1`, made).Scan(&by); err != nil || by != nil {
		t.Fatalf("created_by after the user went: %v %v, want NULL", by, err)
	}
}

func TestOrderNotesRLS(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	note := func(user, org, order uuid.UUID, author uuid.UUID) (uuid.UUID, error) {
		id := uuid.New()
		_, err := f.asApp(user, org, `insert into order_notes(id, organizer_id, order_id, author, body) values ($1, $2, $3, $4, 'Paid at the door')`,
			id, org, order, author)
		return id, err
	}
	count := func(user, org uuid.UUID) int {
		t.Helper()
		var n int
		if err := db.WithOrganizer(ctx, f.pool, user, org, func(tx pgx.Tx, _ string) error {
			return tx.QueryRow(ctx, `select count(*) from order_notes`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const clear = `update order_notes set body = '', deleted_at = now() where id = $1`

	ownerNote, err := note(f.ua, f.oa, f.orderA, f.ua)
	if err != nil {
		t.Fatalf("owner adds a note: %v", err)
	}
	_, err = note(f.ua, f.oa, f.orderA, f.admin)
	wantCode(t, "owner writes as the admin", err, "42501")
	if n := count(f.admin, f.oa); n != 1 {
		t.Errorf("admin sees %d notes, want 1", n)
	}
	if n := count(f.checkin, f.oa); n != 0 {
		t.Errorf("check-in member sees %d notes, want 0", n)
	}
	_, err = note(f.checkin, f.oa, f.orderA, f.checkin)
	wantCode(t, "check-in member adds a note", err, "42501")
	if n := count(f.ub, f.ob); n != 0 {
		t.Errorf("organizer B sees %d of A's notes", n)
	}
	// A manager of A can't put a note on B's order (P7).
	_, err = note(f.ua, f.oa, f.orderB, f.ua)
	if err == nil {
		t.Error("A's owner noted B's order under A")
	}
	id := uuid.New()
	_, err = f.asApp(f.ua, f.oa, `insert into order_notes(id, organizer_id, order_id, author, body) values ($1, $2, $3, $4, 'x')`, id, f.ob, f.orderB, f.ua)
	wantCode(t, "A's owner writes a note into B", err, "42501")
	_, err = f.asApp(f.ua, f.oa, `insert into order_notes(id, organizer_id, order_id, author, body, created_at) values ($1, $2, $3, $4, 'x', now() - interval '1 year')`,
		uuid.New(), f.oa, f.orderA, f.ua)
	wantCode(t, "back-dated note", err, "42501")

	adminNote, err := note(f.admin, f.oa, f.orderA, f.admin)
	if err != nil {
		t.Fatalf("admin adds a note: %v", err)
	}
	if n, err := f.asApp(f.admin, f.oa, clear, ownerNote); err != nil || n != 0 {
		t.Errorf("admin clears the owner's note: %d %v, want 0 rows", n, err)
	}
	if n, err := f.asApp(f.ua, f.oa, clear, adminNote); err != nil || n != 1 {
		t.Errorf("owner clears the admin's note: %d %v, want 1 row", n, err)
	}
	if n, err := f.asApp(f.ua, f.oa, clear, adminNote); err != nil || n != 0 {
		t.Errorf("clearing a cleared note again: %d %v, want 0 rows", n, err)
	}
	_, err = f.asApp(f.ua, f.oa, `update order_notes set body = 'x' where id = $1`, ownerNote)
	wantCode(t, "owner edits a note", err, "42501")

	// Column grants are the only rights ts_app has (P6): each update
	// below is a valid clear, so only the grant refuses it.
	for _, set := range []string{"author = '" + f.admin.String() + "'", "order_id = '" + f.orderA.String() + "'",
		"organizer_id = '" + f.oa.String() + "'", "created_at = now()"} {
		_, err := f.asApp(f.ua, f.oa, `update order_notes set body = '', deleted_at = now(), `+set+` where id = $1`, ownerNote)
		wantCode(t, "ts_app set "+set, err, "42501")
	}
	_, err = f.asApp(f.ua, f.oa, `delete from order_notes where id = $1`, ownerNote)
	wantCode(t, "ts_app deletes a note", err, "42501")

	// The system writes a note for a manager it vouches for, nobody else.
	sys := `insert into order_notes(id, organizer_id, order_id, author, body) values ($1, $2, $3, $4, 'Comp for the band')`
	if _, err := f.asSystem(f.oa, sys, uuid.New(), f.oa, f.orderA, f.admin); err != nil {
		t.Errorf("ts_system note by the admin: %v", err)
	}
	_, err = f.asSystem(f.oa, sys, uuid.New(), f.oa, f.orderA, f.checkin)
	wantCode(t, "ts_system note by the check-in member", err, "42501")
	_, err = f.asSystem(f.ob, sys, uuid.New(), f.ob, f.orderB, f.ua)
	wantCode(t, "ts_system note on B by A's owner", err, "42501")
	_, err = f.asSystem(f.oa, `select count(*) from order_notes`)
	wantCode(t, "ts_system reads notes", err, "42501")
}

func TestAuditSystemMember(t *testing.T) {
	f := newOrderToolsFixture(t)
	row := func(actor uuid.UUID, action, entity string, staffSession *uuid.UUID) error {
		_, err := f.asSystem(f.oa, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id, actor_staff_session_id)
			values ($1, $2, $3, $4, $5, $6)`, f.oa, actor, action, entity, f.orderA, staffSession)
		return err
	}
	if err := row(f.ua, "order.created_manual", "order", nil); err != nil {
		t.Errorf("created_manual as the owner: %v", err)
	}
	if err := row(f.admin, "order.note_added", "order", nil); err != nil {
		t.Errorf("note_added as the admin: %v", err)
	}
	wantCode(t, "order.refunded as the owner", row(f.ua, "order.refunded", "order", nil), "42501")
	wantCode(t, "created_manual as the check-in member", row(f.checkin, "order.created_manual", "order", nil), "42501")
	wantCode(t, "created_manual as B's owner", row(f.ub, "order.created_manual", "order", nil), "42501")
	wantCode(t, "created_manual on a ticket", row(f.ua, "order.created_manual", "ticket", nil), "42501")
	staff := uuid.New() // RLS refuses before the foreign key would
	wantCode(t, "created_manual with a staff session", row(f.ua, "order.created_manual", "order", &staff), "42501")
	_, err := f.asSystem(f.oa, `insert into audit_log(organizer_id, actor_user_id, actor_system, action, entity_type, entity_id)
		values ($1, null, false, 'order.created_manual', 'order', $2)`, f.oa, f.orderA)
	wantCode(t, "created_manual by nobody", err, "42501")
	if _, err := f.asSystem(f.oa, `insert into audit_log(organizer_id, actor_system, action, entity_type, entity_id)
		values ($1, true, 'order.refunded', 'order', $2)`, f.oa, f.orderA); err != nil {
		t.Errorf("system row: %v", err)
	}
}

func TestSetOrderAlerts(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	set := func(p db.Principal, on bool) (bool, error) {
		var ok bool
		err := db.WithPrincipal(ctx, f.pool, p, f.oa, func(tx pgx.Tx, _ string) error {
			return tx.QueryRow(ctx, `select set_order_alerts($1)`, on).Scan(&ok)
		})
		return ok, err
	}
	alerts := func(user uuid.UUID) bool {
		t.Helper()
		var on bool
		if err := f.owner.QueryRow(ctx, `select order_alerts from memberships where organizer_id = $1 and user_id = $2`, f.oa, user).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	if ok, err := set(db.Principal{UserID: f.ua}, true); err != nil || !ok || !alerts(f.ua) {
		t.Errorf("owner on: %v %v, column %v", ok, err, alerts(f.ua))
	}
	if ok, err := set(db.Principal{UserID: f.checkin}, true); err != nil || ok || alerts(f.checkin) {
		t.Errorf("check-in on: %v %v, column %v; want false, unchanged", ok, err, alerts(f.checkin))
	}
	key := seedKey(t, f.pool, f.oa, "write", false)
	if ok, err := set(db.Principal{KeyID: key, KeyOrg: f.oa, KeyScope: "write"}, false); err != nil || ok || !alerts(f.ua) {
		t.Errorf("key off: %v %v, owner's column %v; want false, unchanged", ok, err, alerts(f.ua))
	}
	if ok, err := set(db.Principal{UserID: f.ua}, false); err != nil || !ok || alerts(f.ua) {
		t.Errorf("owner off: %v %v, column %v", ok, err, alerts(f.ua))
	}
}

type claim struct {
	user uuid.UUID
	nth  int
}

func (f orderToolsFixture) claim(t *testing.T, org uuid.UUID, unscoped bool) []claim {
	t.Helper()
	ctx := context.Background()
	var out []claim
	if err := system.Run(ctx, f.pool, org, func(tx pgx.Tx) error {
		if unscoped {
			if _, err := tx.Exec(ctx, `select set_config('app.organizer_id', '', true)`); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `select user_id, nth from claim_order_alerts()`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (claim, error) {
			var c claim
			return c, r.Scan(&c.user, &c.nth)
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(out, func(a, b claim) int { return strings.Compare(a.user.String(), b.user.String()) })
	return out
}

func TestClaimOrderAlertsCap(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `update memberships set order_alerts = true where organizer_id = $1`, f.oa); err != nil {
		t.Fatal(err)
	}
	want := []claim{{f.ua, 1}, {f.admin, 1}}
	slices.SortFunc(want, func(a, b claim) int { return strings.Compare(a.user.String(), b.user.String()) })
	if got := f.claim(t, f.oa, false); !slices.Equal(got, want) {
		t.Fatalf("first claim %v, want %v (owner and admin, never check-in)", got, want)
	}
	for i := 2; i <= 50; i++ {
		got := f.claim(t, f.oa, false)
		if len(got) != 2 || got[0].nth != i || got[1].nth != i {
			t.Fatalf("claim %d: %v", i, got)
		}
	}
	if got := f.claim(t, f.oa, false); len(got) != 0 {
		t.Fatalf("51st claim: %v, want none", got)
	}
	if got := f.claim(t, f.ob, false); len(got) != 0 {
		t.Fatalf("B, nobody opted in: %v", got)
	}
	if _, err := f.owner.Exec(ctx, `update order_alert_counts set day = day - 1`); err != nil {
		t.Fatal(err)
	}
	if got := f.claim(t, f.oa, false); len(got) != 2 || got[0].nth != 1 || got[1].nth != 1 {
		t.Fatalf("next day: %v, want both at 1", got)
	}
	var old int
	if err := f.owner.QueryRow(ctx, `select count(*) from order_alert_counts where day < (statement_timestamp() at time zone 'UTC')::date`).Scan(&old); err != nil || old != 0 {
		t.Fatalf("yesterday's rows left: %d %v", old, err)
	}
	if got := f.claim(t, f.oa, true); len(got) != 0 {
		t.Fatalf("unscoped claim: %v, want none", got)
	}
}

func TestOrderAlertRecipient(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	recipient := func(org, user uuid.UUID) *string {
		t.Helper()
		var email *string
		if err := system.Run(ctx, f.pool, org, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select order_alert_recipient($1)`, user).Scan(&email)
		}); err != nil {
			t.Fatal(err)
		}
		return email
	}
	if _, err := f.owner.Exec(ctx, `update memberships set order_alerts = true where organizer_id = $1`, f.oa); err != nil {
		t.Fatal(err)
	}
	if got := recipient(f.oa, f.ua); got == nil || *got != "a@example.com" {
		t.Errorf("opted-in owner: %s, want a@example.com", str(got))
	}
	if got := recipient(f.oa, f.checkin); got != nil {
		t.Errorf("check-in member with alerts forced on: %s, want NULL", *got)
	}
	if got := recipient(f.ob, f.ua); got != nil {
		t.Errorf("A's owner in B's scope: %s, want NULL", *got)
	}
	if _, err := f.owner.Exec(ctx, `update memberships set order_alerts = false where user_id = $1`, f.ua); err != nil {
		t.Fatal(err)
	}
	if got := recipient(f.oa, f.ua); got != nil {
		t.Errorf("opted-out owner: %s, want NULL", *got)
	}
	if _, err := f.owner.Exec(ctx, `delete from memberships where user_id = $1`, f.admin); err != nil {
		t.Fatal(err)
	}
	if got := recipient(f.oa, f.admin); got != nil {
		t.Errorf("removed admin: %s, want NULL", *got)
	}
}

func TestOrderByLinkSource(t *testing.T) {
	f := newOrderToolsFixture(t)
	ctx := context.Background()
	manual, err := f.order(f.oa, f.eventA, "manual", ptr("invoice"), 4000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		order          uuid.UUID
		source, method string
	}{{manual, "manual", "invoice"}, {f.orderA, "online", "<nil>"}} {
		var code, source string
		var method *string
		if err := f.owner.QueryRow(ctx, `select code from orders where id = $1`, c.order).Scan(&code); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(ctx, `select source, manual_method from order_by_link($1, sha256($1::text::bytea))`, code).Scan(&source, &method); err != nil {
			t.Fatal(err)
		}
		if source != c.source || str(method) != c.method {
			t.Errorf("%s: %s %s, want %s %s", code, source, str(method), c.source, c.method)
		}
	}
}
