package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// staffTables are readable by every live staff user in any scope, by design
// (their own session list), so the zero-rows walks leave them out and
// TestStaffLogTablesShowTheirOrganizer covers them. The takedown tables are
// left out too: no app role may read them at all (TestCatalogHardening).
var staffTables = map[string]bool{"staff_sessions": true, "staff_access_log": true,
	"organizer_suspensions": true, "staff_actions": true}

type staffFixture struct {
	pool        *pgxpool.Pool
	owner       *pgxpool.Pool
	ua, oa, ob  uuid.UUID // A's owner, organizers A and B
	staff       uuid.UUID // S
	login       uuid.UUID // S's MFA login session
	staffSessA  uuid.UUID // S's live staff session on A
	refundA     uuid.UUID // a failed refund on A
	orderCountA int
}

func seedLogin(t *testing.T, owner *pgxpool.Pool, user uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := owner.QueryRow(context.Background(), `insert into sessions(user_id, token_hash, mfa, expires_at)
		values ($1, sha256(gen_random_uuid()::text::bytea), true, now() + interval '1 hour') returning id`, user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedStaff(t *testing.T, owner *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := owner.QueryRow(context.Background(), `with u as (insert into users(email, name, email_verified_at) values ($1, 'Staff', now()) returning id)
		insert into staff(user_id, granted_by) select id, 'james' from u returning user_id`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// startStaff calls start_staff_session as the app, the way staff mode will.
func startStaff(ctx context.Context, pool *pgxpool.Pool, user, login uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.WithUser(ctx, pool, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select start_staff_session($1, $2, 'Checking a refund report', '192.0.2.1', 'test')`, login, slug).Scan(&id)
	})
	return id, err
}

// newStaffFixture seeds organizers A and B, each with a sale, and a staff
// user S with an MFA login session and a live staff session on A.
func newStaffFixture(t *testing.T) staffFixture {
	t.Helper()
	ctx := context.Background()
	f := staffFixture{pool: testdb.New(t)}
	f.owner = testdb.Owner(t, f.pool)
	f.ua, f.oa = seedOrg(t, f.pool, "a@example.com", "org-a")
	_, f.ob = seedOrg(t, f.pool, "b@example.com", "org-b")
	seedSale(t, f.pool, f.oa)
	if _, err := f.owner.Exec(ctx, `update orders set code = 'TS-AAAAA'; update payments set payment_intent_id = 'pi_a';
		update checkouts set token_hash = sha256('tok-a');
		update refunds set status = 'failed', failure = 'stripe_failed'`); err != nil {
		t.Fatal(err)
	}
	seedSale(t, f.pool, f.ob)
	if err := f.owner.QueryRow(ctx, `select id from refunds where organizer_id = $1`, f.oa).Scan(&f.refundA); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRow(ctx, `select count(*) from orders where organizer_id = $1`, f.oa).Scan(&f.orderCountA); err != nil || f.orderCountA == 0 {
		t.Fatalf("orders on A: %d %v", f.orderCountA, err)
	}
	f.staff = seedStaff(t, f.owner, "s@example.com")
	f.login = seedLogin(t, f.owner, f.staff)
	var err error
	if f.staffSessA, err = startStaff(ctx, f.pool, f.staff, f.login, "org-a"); err != nil {
		t.Fatal(err)
	}
	return f
}

// rlsTables lists every table with row security, but the staff log tables.
func rlsTables(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select tablename::text from pg_tables where schemaname = 'public' and rowsecurity order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range names {
		if !staffTables[n] {
			out = append(out, n)
		}
	}
	if len(out) < 20 {
		t.Fatalf("row-security tables: %v", out)
	}
	return out
}

// seesNothing fails the test for every RLS table tx can read a row of.
func seesNothing(ctx context.Context, t *testing.T, tx pgx.Tx, tables []string) {
	t.Helper()
	for _, table := range tables {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s: saw %d rows, want 0", table, n)
		}
	}
}

// handScope opens a transaction with the GUCs set by hand, as a coding
// mistake or a forged scope would, and checks it sees nothing.
func handScope(t *testing.T, f staffFixture, mode pgx.TxAccessMode, user, staffSession, org uuid.UUID, key ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tables := rlsTables(t, f.pool)
	err := pgx.BeginTxFunc(ctx, f.pool, pgx.TxOptions{AccessMode: mode}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true),
			set_config('app.staff_session_id', $3, true)`, user.String(), org.String(), nilEmpty(staffSession)); err != nil {
			return err
		}
		for _, k := range key {
			if _, err := tx.Exec(ctx, `select set_config('app.api_key_id', $1, true)`, k.String()); err != nil {
				return err
			}
		}
		seesNothing(ctx, t, tx, tables)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Spec test 1: being staff grants nothing without a staff session.
func TestStaffFlagAloneSeesNothing(t *testing.T) {
	f := newStaffFixture(t)
	handScope(t, f, pgx.ReadOnly, f.staff, uuid.Nil, f.oa)
	if err := db.WithOrganizer(context.Background(), f.pool, f.staff, f.oa, func(pgx.Tx, string) error { return nil }); !errors.Is(err, db.ErrNotMember) {
		t.Fatalf("WithOrganizer as staff: %v, want ErrNotMember", err)
	}
}

// Spec test 2: a staff session on A reads A and nothing of B.
func TestStaffScopeIsOneOrganizer(t *testing.T) {
	f := newStaffFixture(t)
	handScope(t, f, pgx.ReadOnly, f.staff, f.staffSessA, f.ob)
	err := db.WithStaff(context.Background(), f.pool, f.staff, f.staffSessA, f.ob, func(pgx.Tx) error {
		t.Error("WithStaff ran fn for another organizer")
		return nil
	})
	if !errors.Is(err, db.ErrNotMember) {
		t.Fatalf("WithStaff on B: %v, want ErrNotMember", err)
	}

	// app_staff_organizer() itself refuses B, not only app_organizer()'s
	// comparison. Only the owner may call it directly.
	ctx := context.Background()
	for org, want := range map[uuid.UUID]uuid.NullUUID{f.oa: {UUID: f.oa, Valid: true}, f.ob: {}} {
		var got uuid.NullUUID
		err := pgx.BeginTxFunc(ctx, f.owner, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true),
				set_config('app.staff_session_id', $3, true), app_staff_organizer()`,
				f.staff.String(), org.String(), f.staffSessA.String()).Scan(nil, nil, nil, &got)
		})
		if err != nil || got != want {
			t.Errorf("app_staff_organizer() scoped to %v: %v %v, want %v", org, got, err, want)
		}
	}
}

// A staff scope never rides alongside an API key, even a live one on A.
func TestStaffScopeRefusesAKey(t *testing.T) {
	f := newStaffFixture(t)
	handScope(t, f, pgx.ReadOnly, f.staff, f.staffSessA, f.oa, seedKey(t, f.pool, f.oa, "read", false))
}

// Spec test 3: the staff GUCs in a read-write transaction count for nothing.
func TestStaffScopeNeedsReadOnly(t *testing.T) {
	f := newStaffFixture(t)
	handScope(t, f, pgx.ReadWrite, f.staff, f.staffSessA, f.oa)
}

// Spec test 4: another user, even another staff user with an MFA login of
// their own, can't borrow S's staff session.
func TestStaffScopeIsOneUser(t *testing.T) {
	f := newStaffFixture(t)
	y := seedStaff(t, f.owner, "y@example.com")
	seedLogin(t, f.owner, y)
	handScope(t, f, pgx.ReadOnly, y, f.staffSessA, f.oa)
	if err := db.WithStaff(context.Background(), f.pool, y, f.staffSessA, f.oa, func(pgx.Tx) error { return nil }); !errors.Is(err, db.ErrNotMember) {
		t.Fatalf("WithStaff as Y: %v, want ErrNotMember", err)
	}
}

// Spec test 5: ending, expiry, revocation, logout and losing MFA each bite on
// the very next statement of an open staff transaction.
func TestStaffScopeEndsMidTransaction(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	tables := rlsTables(t, f.pool)
	sess, user, login := f.staffSessA, f.staff, f.login
	for _, c := range []struct {
		name      string
		cut, undo string
		arg       uuid.UUID
	}{
		{"ended", `update staff_sessions set ended_at = now(), ended_how = 'staff' where id = $1`,
			`update staff_sessions set ended_at = null, ended_how = null where id = $1`, sess},
		{"expired", `update staff_sessions set started_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' where id = $1`,
			`update staff_sessions set started_at = now(), expires_at = now() + interval '59 minutes' where id = $1`, sess},
		{"revoked", `update staff set revoked_at = now() where user_id = $1`,
			`update staff set revoked_at = null where user_id = $1`, user},
		{"logged out", `delete from sessions where id = $1`,
			`insert into sessions(id, user_id, token_hash, mfa, expires_at)
			 values ($1, '` + user.String() + `', sha256(gen_random_uuid()::text::bytea), true, now() + interval '1 hour')`, login},
		{"mfa lost", `update sessions set mfa = false where id = $1`,
			`update sessions set mfa = true where id = $1`, login},
		{"login expired", `update sessions set expires_at = now() - interval '1 second' where id = $1`,
			`update sessions set expires_at = now() + interval '1 hour' where id = $1`, login},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := db.WithStaff(ctx, f.pool, user, sess, f.oa, func(tx pgx.Tx) error {
				var n int
				if err := tx.QueryRow(ctx, `select count(*) from orders`).Scan(&n); err != nil || n != f.orderCountA {
					t.Fatalf("orders before: %d %v, want %d", n, err, f.orderCountA)
				}
				if _, err := f.owner.Exec(ctx, c.cut, c.arg); err != nil {
					t.Fatal(err)
				}
				seesNothing(ctx, t, tx, tables)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			err = db.WithUser(ctx, f.pool, user, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `select log_staff_access($1, 'GET', 'listOrders')`, sess)
				return err
			})
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "42501" {
				t.Errorf("log_staff_access after the cut: %v, want 42501", err)
			}
			if _, err := f.owner.Exec(ctx, c.undo, c.arg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Spec test 6: a valid staff scope writes nothing. Direct writes hit the
// read-only transaction (25006); the member-only functions refuse without
// an error and change no row.
func TestStaffCannotWrite(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `insert into users(email, name, email_verified_at) values ('c@example.com', 'C', now())`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var s string
		if err := f.owner.QueryRow(ctx, `select (select count(*) from events)::text || '/' ||
			(select string_agg(status || email::text, ',' order by id) from orders) || '/' ||
			(select string_agg(status, ',' order by id) from refunds) || '/' ||
			(select string_agg(user_id::text || role, ',' order by user_id) from memberships)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	asStaff := func(q string, args ...any) (string, error) {
		var r string
		err := db.WithStaff(ctx, f.pool, f.staff, f.staffSessA, f.oa, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, q, args...).Scan(&r)
		})
		return r, err
	}
	for _, q := range []string{
		`insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
		 values ((select app_organizer()), 'staff-show', 'Show', 'UTC', now(), now() + interval '1 hour') returning id::text`,
		`update orders set email = 'x@example.com' returning id::text`,
	} {
		_, err := asStaff(q)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "25006" {
			t.Errorf("%s: %v, want 25006", q, err)
		}
	}
	for q, want := range map[string]string{
		`select retry_refund('` + f.refundA.String() + `')::text`:   "false",
		`select add_member('c@example.com', 'checkin')`:             "forbidden",
		`select result from remove_member('` + f.ua.String() + `')`: "forbidden",
	} {
		if r, err := asStaff(q); err != nil || r != want {
			t.Errorf("%s: %q %v, want %q", q, r, err, want)
		}
	}
	if after := snapshot(); after != before {
		t.Fatalf("rows changed:\n%s\n%s", before, after)
	}
}

// Spec test 7: ts_app can't touch staff at all, and only reads the logs.
func TestAppCannotTouchStaffTables(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	for _, q := range []string{
		`select count(*) from staff`,
		`insert into staff(user_id, granted_by) values (gen_random_uuid(), 'x')`,
		`update staff set revoked_at = null`,
		`delete from staff`,
		`insert into staff_sessions(user_id, session_id, organizer_slug, organizer_name, reason, expires_at)
		 values (gen_random_uuid(), gen_random_uuid(), 'org-a', 'Org', 'just looking around', now())`,
		`update staff_sessions set ended_at = null`,
		`delete from staff_sessions`,
		`insert into staff_access_log(staff_session_id, method, route) values (gen_random_uuid(), 'GET', 'x')`,
		`update staff_access_log set route = 'x'`,
		`delete from staff_access_log`,
	} {
		err := db.WithUser(ctx, f.pool, f.staff, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%s: %v, want 42501", q, err)
		}
	}
}

// Spec test 8: no staff session outlives an hour, even one the owner writes.
func TestStaffSessionCapsAtSixtyMinutes(t *testing.T) {
	f := newStaffFixture(t)
	_, err := f.owner.Exec(context.Background(), `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, started_at, expires_at)
		values ($1, gen_random_uuid(), $2, 'org-a', 'Org', 'Checking a refund report', now(), now() + interval '61 minutes')`, f.staff, f.oa)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("61-minute session: %v, want 23514", err)
	}
}

// P6: staff read every staff session and request; a member reads only the
// ones on their own organizer.
func TestStaffLogTablesShowTheirOrganizer(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	login2 := seedLogin(t, f.owner, f.staff)
	if _, err := startStaff(ctx, f.pool, f.staff, login2, "org-b"); err != nil {
		t.Fatal(err)
	}
	for _, login := range []uuid.UUID{f.login, login2} {
		var sess uuid.UUID
		if err := f.owner.QueryRow(ctx, `select id from staff_sessions where session_id = $1`, login).Scan(&sess); err != nil {
			t.Fatal(err)
		}
		if err := db.WithUser(ctx, f.pool, f.staff, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select log_staff_access($1, 'GET', 'listOrders')`, sess)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(tx pgx.Tx) (orgs []uuid.UUID) {
		rows, err := tx.Query(ctx, `select organizer_id from staff_sessions union all select organizer_id from staff_access_log order by 1`)
		if err != nil {
			t.Fatal(err)
		}
		orgs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatal(err)
		}
		return orgs
	}
	if err := db.WithOrganizer(ctx, f.pool, f.ua, f.oa, func(tx pgx.Tx, _ string) error {
		got := count(tx)
		if len(got) != 2 || got[0] != f.oa || got[1] != f.oa {
			t.Errorf("A's owner sees %v, want A's one session and one request", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(ctx, f.pool, f.ua, func(tx pgx.Tx) error {
		if got := count(tx); len(got) != 0 {
			t.Errorf("A's owner with no organizer scope sees %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(ctx, f.pool, f.staff, func(tx pgx.Tx) error {
		if got := count(tx); len(got) != 4 {
			t.Errorf("staff see %v, want both sessions and both requests", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `update staff set revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(ctx, f.pool, f.staff, func(tx pgx.Tx) error {
		if got := count(tx); len(got) != 0 {
			t.Errorf("revoked staff see %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// S1: a member can't write an audit row that claims to be staff.
func TestMembersCannotForgeStaffAudit(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	err := db.WithOrganizer(ctx, f.pool, f.ua, f.oa, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id)
			values ($1, $2, $3, 'staff.session_started', 'staff_session', $3)`, f.oa, f.staff, f.staffSessA)
		return err
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("member writing a staff audit row: %v, want 42501", err)
	}
}

// The definer functions refuse everything but their one job.
func TestStaffFunctionsRefuseStrangers(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	var pg *pgconn.PgError
	code := func(err error) string {
		if errors.As(err, &pg) {
			return pg.Code
		}
		return ""
	}

	// start: the audit row, the snapshot, and the refusals.
	var name, slug, after string
	if err := f.owner.QueryRow(ctx, `select ss.organizer_name, ss.organizer_slug::text, a.after->>'reason' from staff_sessions ss
		join audit_log a on a.actor_staff_session_id = ss.id and a.actor_user_id = ss.user_id and a.action = 'staff.session_started'
		where ss.id = $1 and a.organizer_id = $2`, f.staffSessA, f.oa).Scan(&name, &slug, &after); err != nil || name != "Org" || slug != "org-a" || after != "Checking a refund report" {
		t.Fatalf("started session: %q %q %q %v", name, slug, after, err)
	}
	if _, err := startStaff(ctx, f.pool, f.staff, f.login, "no-such-org"); code(err) != "P0002" {
		t.Errorf("unknown slug: %v, want no_data_found", err)
	}
	if _, err := startStaff(ctx, f.pool, f.ua, seedLogin(t, f.owner, f.ua), "org-a"); code(err) != "42501" {
		t.Errorf("non-staff: %v, want 42501", err)
	}
	y := seedStaff(t, f.owner, "y@example.com")
	if _, err := startStaff(ctx, f.pool, y, f.login, "org-a"); code(err) != "42501" {
		t.Errorf("someone else's login session: %v, want 42501", err)
	}
	noMFA := seedLogin(t, f.owner, f.staff)
	if _, err := f.owner.Exec(ctx, `update sessions set mfa = false where id = $1`, noMFA); err != nil {
		t.Fatal(err)
	}
	if _, err := startStaff(ctx, f.pool, f.staff, noMFA, "org-a"); code(err) != "42501" {
		t.Errorf("login without MFA: %v, want 42501", err)
	}

	// log: only the session's own user, only while live.
	logAs := func(user uuid.UUID) error {
		return db.WithUser(ctx, f.pool, user, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select log_staff_access($1, 'GET', 'listOrders')`, f.staffSessA)
			return err
		})
	}
	if err := logAs(y); code(err) != "42501" {
		t.Errorf("logging someone else's session: %v, want 42501", err)
	}

	// end: only staff or logout, only the session's own user, once.
	endAs := func(user uuid.UUID, how string) error {
		return db.WithUser(ctx, f.pool, user, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select end_staff_session($1, $2, 'done')`, f.staffSessA, how)
			return err
		})
	}
	if err := endAs(f.staff, "revoked"); code(err) != "23514" {
		t.Errorf("ending as revoked: %v, want 23514", err)
	}
	if err := endAs(y, "staff"); err != nil {
		t.Fatal(err)
	}
	if err := logAs(f.staff); err != nil {
		t.Fatalf("Y ended S's session: %v", err)
	}
	for range 2 {
		if err := endAs(f.staff, "staff"); err != nil {
			t.Fatal(err)
		}
	}
	if err := logAs(f.staff); code(err) != "42501" {
		t.Errorf("logging an ended session: %v, want 42501", err)
	}
	var ended int
	if err := f.owner.QueryRow(ctx, `select count(*) from audit_log where action = 'staff.session_ended' and actor_staff_session_id = $1`, f.staffSessA).Scan(&ended); err != nil || ended != 1 {
		t.Fatalf("session_ended rows: %d %v, want 1", ended, err)
	}

	// The sweeper closes what's past its hour, as the staff user.
	if _, err := f.owner.Exec(ctx, `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, started_at, expires_at)
		values ($1, gen_random_uuid(), $2, 'org-b', 'Org', 'Checking a refund report', now() - interval '2 hours', now() - interval '1 hour')`, f.staff, f.ob); err != nil {
		t.Fatal(err)
	}
	var closed int
	if err := f.pool.QueryRow(ctx, `select close_expired_staff_sessions(10)`).Scan(&closed); err != nil || closed != 1 {
		t.Fatalf("closed: %d %v, want 1", closed, err)
	}
	if err := f.owner.QueryRow(ctx, `select count(*) from audit_log a join staff_sessions ss on ss.id = a.actor_staff_session_id
		where a.action = 'staff.session_ended' and a.organizer_id = $1 and a.actor_user_id = $2 and not a.actor_system
		  and ss.ended_how = 'expired' and ss.ended_at = ss.expires_at`, f.ob, f.staff).Scan(&ended); err != nil || ended != 1 {
		t.Fatalf("expired rows: %d %v, want 1", ended, err)
	}

	// Search: staff only. Directory: the scoped organizer's staff only.
	search := func(user uuid.UUID) (n int) {
		if err := db.WithUser(ctx, f.pool, user, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select count(*) from staff_search_organizers('org-')`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if s, u := search(f.staff), search(f.ua); s != 2 || u != 0 {
		t.Errorf("search: staff %d, member %d; want 2 and 0", s, u)
	}
	dir := func(org uuid.UUID) (ids []uuid.UUID) {
		if err := db.WithOrganizer(ctx, f.pool, f.ua, org, func(tx pgx.Tx, _ string) error {
			rows, err := tx.Query(ctx, `select user_id from app_staff_directory()`)
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	yLogin := seedLogin(t, f.owner, y)
	if _, err := startStaff(ctx, f.pool, y, yLogin, "org-b"); err != nil {
		t.Fatal(err)
	}
	if got := dir(f.oa); len(got) != 1 || got[0] != f.staff {
		t.Errorf("A's directory: %v, want only S, not Y who was only on B", got)
	}
	var cols []string
	if err := f.owner.QueryRow(ctx, `select array(select unnest(proargnames) from pg_proc
		where oid = 'app_staff_directory()'::regprocedure)`).Scan(&cols); err != nil || len(cols) != 2 || cols[0] != "user_id" || cols[1] != "name" {
		t.Errorf("app_staff_directory returns %v %v, want user_id and name only", cols, err)
	}
	if err := db.WithUser(ctx, f.pool, f.ua, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from app_staff_directory()`).Scan(&n); err != nil || n != 0 {
			t.Errorf("unscoped directory: %d %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Starting staff mode again on the same login session ends the live one,
// as ended by staff; one that ran out unswept is recorded as expired, at
// its expiry.
func TestStartStaffSessionReplacesTheLiveOne(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	ended := func(id uuid.UUID) (how, auditHow string, atExpiry bool) {
		if err := f.owner.QueryRow(ctx, `select ss.ended_how, a.after->>'how', ss.ended_at = ss.expires_at
			from staff_sessions ss join audit_log a on a.actor_staff_session_id = ss.id and a.action = 'staff.session_ended'
			where ss.id = $1`, id).Scan(&how, &auditHow, &atExpiry); err != nil {
			t.Fatal(err)
		}
		return how, auditHow, atExpiry
	}
	second, err := startStaff(ctx, f.pool, f.staff, f.login, "org-b")
	if err != nil {
		t.Fatal(err)
	}
	if how, audit, atExpiry := ended(f.staffSessA); how != "staff" || audit != "staff" || atExpiry {
		t.Errorf("replaced live session: %s %s at expiry %v, want staff", how, audit, atExpiry)
	}
	if _, err := f.owner.Exec(ctx, `update staff_sessions set started_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' where id = $1`, second); err != nil {
		t.Fatal(err)
	}
	third, err := startStaff(ctx, f.pool, f.staff, f.login, "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if how, audit, atExpiry := ended(second); how != "expired" || audit != "expired" || !atExpiry {
		t.Errorf("replaced expired session: %s %s at expiry %v, want expired", how, audit, atExpiry)
	}
	if err := db.WithStaff(ctx, f.pool, f.staff, third, f.oa, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("the new session: %v", err)
	}
}

// The access log is append-only and nobody trims it, so a bug can't bloat it.
func TestStaffAccessLogBoundsItsRows(t *testing.T) {
	f := newStaffFixture(t)
	for _, c := range []struct{ method, route string }{
		{"GETGETGETGE", "listOrders"},
		{"GET", strings.Repeat("r", 201)},
	} {
		_, err := f.owner.Exec(context.Background(), `insert into staff_access_log(staff_session_id, organizer_id, method, route)
			values ($1, $2, $3, $4)`, f.staffSessA, f.oa, c.method, c.route)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Errorf("method %d chars, route %d chars: %v, want 23514", len(c.method), len(c.route), err)
		}
	}
	if _, err := f.owner.Exec(context.Background(), `insert into staff_access_log(staff_session_id, organizer_id, method, route)
		values ($1, $2, 'DELETE', $3)`, f.staffSessA, f.oa, strings.Repeat("r", 200)); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
}
