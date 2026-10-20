package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

const takedownReason = "Selling tickets to an illegal event"

// staffAct calls staff_act as the app, the way the staff action handler will.
func staffAct(ctx context.Context, pool *pgxpool.Pool, user, staffSess, login uuid.UUID, kind string, event uuid.NullUUID, reason string) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.WithUser(ctx, pool, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select staff_act($1, $2, $3, $4, $5)`, staffSess, login, kind, event, reason).Scan(&id)
	})
	return id, err
}

// act is staffAct for the fixture's staff user on A.
func (f staffFixture) act(t *testing.T, kind string, event uuid.NullUUID) uuid.UUID {
	t.Helper()
	id, err := staffAct(context.Background(), f.pool, f.staff, f.staffSessA, f.login, kind, event, takedownReason)
	if err != nil {
		t.Fatalf("staff_act %s: %v", kind, err)
	}
	return id
}

func (f staffFixture) eventOf(t *testing.T, org uuid.UUID) uuid.NullUUID {
	t.Helper()
	var id uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `select id from events where organizer_id = $1 limit 1`, org).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return uuid.NullUUID{UUID: id, Valid: true}
}

func pgCode(err error) (code, msg string) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code, pg.Message
	}
	return "", ""
}

// Spec test 16: no app role touches the suspension tables at all.
func TestAppCannotTouchSuspensionTables(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	runs := map[string]func(q string) error{
		"ts_app": func(q string) error {
			return db.WithOrganizer(ctx, f.pool, f.ua, f.oa, func(tx pgx.Tx, _ string) error {
				_, err := tx.Exec(ctx, q)
				return err
			})
		},
		"ts_public": func(q string) error {
			return system.Buyer(ctx, f.pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q)
				return err
			})
		},
		"ts_system": func(q string) error {
			return system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q)
				return err
			})
		},
	}
	for role, run := range runs {
		for _, q := range []string{
			`select count(*) from organizer_suspensions`,
			`insert into organizer_suspensions(organizer_id, staff_action_id) values ('` + f.oa.String() + `', gen_random_uuid())`,
			`update organizer_suspensions set suspended_at = now()`,
			`delete from organizer_suspensions`,
			`select count(*) from staff_actions`,
			`insert into staff_actions(staff_session_id, user_id, organizer_id, kind, reason)
			 values ('` + f.staffSessA.String() + `', '` + f.staff.String() + `', '` + f.oa.String() + `', 'lift', 'just looking around')`,
			`update staff_actions set finished_at = now()`,
			`delete from staff_actions`,
		} {
			if code, _ := pgCode(run(q)); code != "42501" {
				t.Errorf("%s: %s: %v, want 42501", role, q, code)
			}
		}
	}
}

// actionRows counts every row a staff action could leave behind.
func actionRows(t *testing.T, owner *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := owner.QueryRow(context.Background(), `select (select count(*) from staff_actions)::text || '/' ||
		(select count(*) from organizer_suspensions)::text || '/' ||
		(select count(*) from audit_log)::text || '/' || (select count(*) from jobs)::text`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Spec test 17: staff_act re-checks everything and refuses without leaving
// a row anywhere.
func TestStaffActRefusals(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	y := seedStaff(t, f.owner, "y@example.com")
	loginY := seedLogin(t, f.owner, y)
	login2 := seedLogin(t, f.owner, f.staff) // S's, but not the staff session's
	sess, user, login := f.staffSessA, f.staff, f.login
	type refusal struct {
		name       string
		cut, undo  string
		arg        uuid.UUID
		user       uuid.UUID
		login      uuid.UUID
		kind       string
		event      uuid.NullUUID
		reason     string
		code, text string
	}
	base := func(r refusal) refusal {
		if r.user == uuid.Nil {
			r.user = user
		}
		if r.login == uuid.Nil {
			r.login = login
		}
		if r.kind == "" {
			r.kind = "suspend"
		}
		if r.reason == "" {
			r.reason = takedownReason
		}
		return r
	}
	run := func(cases []refusal) {
		for _, c := range cases {
			c = base(c)
			t.Run(c.name, func(t *testing.T) {
				if c.cut != "" {
					if _, err := f.owner.Exec(ctx, c.cut, c.arg); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := f.owner.Exec(ctx, c.undo, c.arg); err != nil {
							t.Fatal(err)
						}
					}()
				}
				before := actionRows(t, f.owner)
				_, err := staffAct(ctx, f.pool, c.user, sess, c.login, c.kind, c.event, c.reason)
				code, msg := pgCode(err)
				if code != c.code || !strings.Contains(msg, c.text) {
					t.Errorf("got %v, want %s %q", err, c.code, c.text)
				}
				if after := actionRows(t, f.owner); after != before {
					t.Errorf("rows changed: %s -> %s", before, after)
				}
			})
		}
	}
	run([]refusal{
		{name: "no staff row", user: f.ua, code: "42501", text: "not allowed"},
		{name: "revoked staff", cut: `update staff set revoked_at = now() where user_id = $1`,
			undo: `update staff set revoked_at = null where user_id = $1`, arg: user, code: "42501", text: "not allowed"},
		{name: "ended staff session", cut: `update staff_sessions set ended_at = now(), ended_how = 'staff' where id = $1`,
			undo: `update staff_sessions set ended_at = null, ended_how = null where id = $1`, arg: sess, code: "42501", text: "not allowed"},
		{name: "expired staff session", cut: `update staff_sessions set started_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' where id = $1`,
			undo: `update staff_sessions set started_at = now(), expires_at = now() + interval '59 minutes' where id = $1`, arg: sess, code: "42501", text: "not allowed"},
		{name: "another user's staff session", user: y, login: loginY, code: "42501", text: "not allowed"},
		{name: "login not mfa", cut: `update sessions set mfa = false where id = $1`,
			undo: `update sessions set mfa = true where id = $1`, arg: login, code: "42501", text: "not allowed"},
		{name: "login expired", cut: `update sessions set expires_at = now() - interval '1 second' where id = $1`,
			undo: `update sessions set expires_at = now() + interval '1 hour' where id = $1`, arg: login, code: "42501", text: "not allowed"},
		{name: "login not the staff session's", login: login2, code: "42501", text: "not allowed"},
		{name: "reason of 9", reason: strings.Repeat("x", 9), code: "23514"},
		{name: "reason of 501", reason: strings.Repeat("x", 501), code: "23514"},
		{name: "lift when not suspended", kind: "lift", code: "55000", text: "not_suspended"},
		{name: "refund when not suspended", kind: "refund_event", event: f.eventOf(t, f.oa), code: "55000", text: "not_suspended"},
	})
	f.act(t, "suspend", uuid.NullUUID{})
	run([]refusal{
		{name: "suspend twice", code: "55000", text: "already_suspended"},
		{name: "event of another organizer", kind: "refund_event", event: f.eventOf(t, f.ob), code: "P0002", text: "no such event"},
		{name: "refund without an event", kind: "refund_event", code: "P0002", text: "no such event"},
	})
}

// Suspend, refund and lift each leave one action, one audit row in the
// organizer's own log naming the staff user and session, and the suspension
// row exactly while suspended.
func TestStaffActSuspendLiftRefund(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	check := func(id uuid.UUID, kind, action string, event uuid.NullUUID, suspended bool) {
		t.Helper()
		var gotKind string
		var gotEvent uuid.NullUUID
		if err := f.owner.QueryRow(ctx, `select kind, event_id from staff_actions
			where id = $1 and staff_session_id = $2 and user_id = $3 and organizer_id = $4 and reason = $5`,
			id, f.staffSessA, f.staff, f.oa, takedownReason).Scan(&gotKind, &gotEvent); err != nil || gotKind != kind || gotEvent != event {
			t.Errorf("staff_actions %s: %s %v %v", kind, gotKind, gotEvent, err)
		}
		var n int
		if err := f.owner.QueryRow(ctx, `select count(*) from audit_log where organizer_id = $1 and action = $2
			and actor_user_id = $3 and actor_staff_session_id = $4 and after->>'reason' = $5`,
			f.oa, action, f.staff, f.staffSessA, takedownReason).Scan(&n); err != nil || n != 1 {
			t.Errorf("audit %s: %d %v, want 1", action, n, err)
		}
		var on bool
		if err := f.owner.QueryRow(ctx, `select exists (select 1 from organizer_suspensions where organizer_id = $1)`, f.oa).Scan(&on); err != nil || on != suspended {
			t.Errorf("after %s suspended %v %v, want %v", kind, on, err, suspended)
		}
	}
	id := f.act(t, "suspend", uuid.NullUUID{})
	check(id, "suspend", "staff.suspended", uuid.NullUUID{}, true)
	var linked uuid.UUID
	if err := f.owner.QueryRow(ctx, `select staff_action_id from organizer_suspensions where organizer_id = $1`, f.oa).Scan(&linked); err != nil || linked != id {
		t.Errorf("suspension names action %v %v, want %v", linked, err, id)
	}
	event := f.eventOf(t, f.oa)
	check(f.act(t, "refund_event", event), "refund_event", "staff.refund_requested", event, true)
	check(f.act(t, "lift", uuid.NullUUID{}), "lift", "staff.lifted", uuid.NullUUID{}, false)
	// staff_act queues nothing itself: the alert is the caller's.
	var jobs int
	if err := f.owner.QueryRow(ctx, `select count(*) from jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Errorf("jobs: %d %v, want 0", jobs, err)
	}
}

// Spec test 18: every member and key of a suspended organizer gets a
// read-only transaction. Writes either hit 25006 or the function's own
// refusal; the owner's add_member and retry_refund reach their writes, so
// 25006 is the proof there. Reads still return rows.
func TestSuspendedScopeIsReadOnly(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	var admin, checkin uuid.UUID
	if err := f.owner.QueryRow(ctx, `with a as (insert into users(email, name, email_verified_at) values ('admin@example.com', 'A', now()) returning id),
			c as (insert into users(email, name, email_verified_at) values ('checkin@example.com', 'C', now()) returning id),
			x as (insert into users(email, name, email_verified_at) values ('c@example.com', 'X', now())),
			m as (insert into memberships(organizer_id, user_id, role) select $1::uuid, id, 'admin' from a union all select $1::uuid, id, 'checkin' from c)
		select (select id from a), (select id from c)`, f.oa).Scan(&admin, &checkin); err != nil {
		t.Fatal(err)
	}
	key := seedKey(t, f.pool, f.oa, "write", false)
	f.act(t, "suspend", uuid.NullUUID{})
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
	for name, p := range map[string]db.Principal{
		"owner":     {UserID: f.ua},
		"admin":     {UserID: admin},
		"checkin":   {UserID: checkin},
		"write key": {KeyID: key, KeyOrg: f.oa, KeyScope: "write"},
	} {
		t.Run(name, func(t *testing.T) {
			do := func(q string) (string, error) {
				var r string
				err := db.WithPrincipal(ctx, f.pool, p, f.oa, func(tx pgx.Tx, _ string) error {
					return tx.QueryRow(ctx, q).Scan(&r)
				})
				return r, err
			}
			for q, refusal := range map[string]string{
				`insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
				 values ((select app_organizer()), 'new-show', 'Show', 'UTC', now(), now() + interval '1 hour') returning id::text`: "",
				`update orders set email = 'x@example.com' returning id::text`: "",
				`select retry_refund('` + f.refundA.String() + `')::text`:      "false",
				`select add_member('c@example.com', 'checkin')`:                "forbidden",
			} {
				// The owner and admin pass retry_refund's check; the owner
				// passes add_member's.
				if name == "owner" || name == "admin" && strings.Contains(q, "retry_refund") {
					refusal = ""
				}
				r, err := do(q)
				if code, _ := pgCode(err); code == "25006" && !errors.Is(err, db.ErrOrganizerSuspended) {
					t.Errorf("%s: %v doesn't say the organizer is suspended", q, err)
				} else if code != "25006" && (refusal == "" || err != nil || r != refusal) {
					t.Errorf("%s: %q %v, want 25006%s", q, r, err, map[bool]string{true: " or " + refusal}[refusal != ""])
				}
			}
			if n, err := do(`select count(*)::text from orders`); err != nil || n == "0" {
				t.Errorf("orders: %s %v, want rows", n, err)
			}
		})
	}
	if after := snapshot(); after != before {
		t.Fatalf("rows changed:\n%s\n%s", before, after)
	}
}

// waitForLockWait returns once some backend of this test's database waits
// on a lock, and fails the test after ten seconds.
func waitForLockWait(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := owner.QueryRow(context.Background(), `select count(*) from pg_locks where not granted
			and pid in (select pid from pg_stat_activity where datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing waited on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Plan edge case 5, through the real WithPrincipal: a member write already
// in flight finishes before the suspension, and one that starts while the
// suspension holds the organizer waits for it and is then read-only.
func TestSuspendWaitsForOrganizerLock(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	rename := func(name string) func(pgx.Tx, string) error {
		return func(tx pgx.Tx, _ string) error {
			_, err := tx.Exec(ctx, `update events set name = $1 where organizer_id = $2`, name, f.oa)
			return err
		}
	}

	t.Run("in-flight write goes first", func(t *testing.T) {
		wrote, release, memberDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		go func() {
			memberDone <- db.WithOrganizer(ctx, f.pool, f.ua, f.oa, func(tx pgx.Tx, role string) error {
				err := rename("Renamed")(tx, role)
				close(wrote)
				<-release
				return err
			})
		}()
		select {
		case <-wrote:
		case err := <-memberDone:
			t.Fatalf("member write: %v", err)
		}
		suspended := make(chan error, 1)
		go func() {
			_, err := staffAct(ctx, f.pool, f.staff, f.staffSessA, f.login, "suspend", uuid.NullUUID{}, takedownReason)
			suspended <- err
		}()
		waitForLockWait(t, f.owner)
		select {
		case err := <-suspended:
			t.Fatalf("suspend finished while a member write was in flight: %v", err)
		default:
		}
		close(release)
		if err := <-memberDone; err != nil {
			t.Fatalf("member write that started first: %v", err)
		}
		if err := <-suspended; err != nil {
			t.Fatalf("suspend after the member committed: %v", err)
		}
	})
	f.act(t, "lift", uuid.NullUUID{})

	t.Run("write started during the suspend waits, then is read-only", func(t *testing.T) {
		staffTx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = staffTx.Rollback(ctx) }()
		if _, err := staffTx.Exec(ctx, `select set_config('app.user_id', $1, true)`, f.staff.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := staffTx.Exec(ctx, `select staff_act($1, $2, 'suspend', null, $3)`, f.staffSessA, f.login, takedownReason); err != nil {
			t.Fatal(err)
		}
		memberDone := make(chan error, 1)
		go func() { memberDone <- db.WithOrganizer(ctx, f.pool, f.ua, f.oa, rename("Again")) }()
		waitForLockWait(t, f.owner)
		select {
		case err := <-memberDone:
			t.Fatalf("member write ran while the suspend held the organizer: %v", err)
		default:
		}
		if err := staffTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if code, _ := pgCode(<-memberDone); code != "25006" {
			t.Fatalf("member write after the suspend committed: %v, want 25006", code)
		}
	})
}

// Spec test 19: the trigger refuses new checkouts and orders whatever the Go
// code does, and stops refusing once lifted.
func TestNoNewSalesWhileSuspended(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	event := f.eventOf(t, f.oa).UUID
	sell := func(n string) error {
		return system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `insert into checkouts(organizer_id, event_id, token_hash, expires_at)
				values ($1, $2, sha256($3::bytea), now() + interval '15 minutes')`, f.oa, event, "tok-new-"+n); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details)
				values ($1, $2, $3, 'b@example.com', 'pending', 0, 0, 0, 0, 0, 'pass', '{}', '{}')`, f.oa, event, "TS-BBBB"+n)
			return err
		})
	}
	f.act(t, "suspend", uuid.NullUUID{})
	if code, msg := pgCode(sell("A")); code != "P0001" || msg != "organizer_suspended" {
		t.Errorf("checkout while suspended: %s %q, want P0001 organizer_suspended", code, msg)
	}
	if code, msg := pgCode(system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details)
			values ($1, $2, 'TS-CCCCB', 'b@example.com', 'pending', 0, 0, 0, 0, 0, 'pass', '{}', '{}')`, f.oa, event)
		return err
	})); code != "P0001" || msg != "organizer_suspended" {
		t.Errorf("order while suspended: %s %q, want P0001 organizer_suspended", code, msg)
	}
	f.act(t, "lift", uuid.NullUUID{})
	if err := sell("D"); err != nil {
		t.Fatalf("sale after lift: %v", err)
	}
}

// Spec test 20, and T13: one claim per refund action, by the system role of
// that organizer, and finish only in the claiming transaction.
func TestClaimStaffRefundOnce(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	f.act(t, "suspend", uuid.NullUUID{})
	event := f.eventOf(t, f.oa)
	action := f.act(t, "refund_event", event)
	claim := func(org, id uuid.UUID, finish bool) error {
		return system.Run(ctx, f.pool, org, func(tx pgx.Tx) error {
			var got uuid.UUID
			if err := tx.QueryRow(ctx, `select claim_staff_refund($1)`, id).Scan(&got); err != nil {
				return err
			}
			if got != event.UUID {
				t.Errorf("claim returned %v, want the event %v", got, event.UUID)
			}
			if finish {
				_, err := tx.Exec(ctx, `select finish_staff_refund($1, '{"orders": 1}')`, id)
				return err
			}
			return nil
		})
	}
	finish := func(id uuid.UUID) error {
		return system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select finish_staff_refund($1, '{}')`, id)
			return err
		})
	}
	if err := claim(f.ob, action, false); err == nil {
		t.Error("claim as the system role of another organizer worked")
	}
	if err := claim(f.oa, action, true); err != nil {
		t.Fatalf("claim and finish: %v", err)
	}
	var result string
	if err := f.owner.QueryRow(ctx, `select result::text from staff_actions where id = $1 and finished_at is not null`, action).Scan(&result); err != nil || result != `{"orders": 1}` {
		t.Errorf("finished action: %q %v", result, err)
	}
	if _, msg := pgCode(claim(f.oa, action, true)); msg != "already_done" {
		t.Errorf("second claim: %q, want already_done", msg)
	}

	second := f.act(t, "refund_event", event)
	if err := finish(second); err == nil {
		t.Error("finish without a claim worked")
	}
	if err := claim(f.oa, second, false); err != nil {
		t.Fatal(err)
	}
	if err := finish(second); err == nil {
		t.Error("finish in a transaction other than the claim's worked")
	}
	f.act(t, "lift", uuid.NullUUID{})
	if _, msg := pgCode(claim(f.oa, second, true)); msg != "not_suspended" {
		t.Errorf("claim after lift: %q, want not_suspended", msg)
	}
	var finished int
	if err := f.owner.QueryRow(ctx, `select count(*) from staff_actions where finished_at is not null`).Scan(&finished); err != nil || finished != 1 {
		t.Errorf("finished actions: %d %v, want 1", finished, err)
	}
	if err := system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select organizer_suspended($1)`, f.oa)
		return err
	}); err != nil {
		t.Errorf("ts_system asking organizer_suspended: %v", err)
	}
}

// A double submit: the second claim waits for the first transaction, then
// finds the action finished. Exactly one wins.
func TestClaimStaffRefundRace(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	f.act(t, "suspend", uuid.NullUUID{})
	action := f.act(t, "refund_event", f.eventOf(t, f.oa))
	claimed, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		firstDone <- system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select claim_staff_refund($1)`, action); err != nil {
				return err
			}
			close(claimed)
			<-release
			_, err := tx.Exec(ctx, `select finish_staff_refund($1, '{}')`, action)
			return err
		})
	}()
	<-claimed
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- system.Run(ctx, f.pool, f.oa, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select claim_staff_refund($1)`, action); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `select finish_staff_refund($1, '{}')`, action)
			return err
		})
	}()
	waitForLockWait(t, f.owner)
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, msg := pgCode(<-secondDone); msg != "already_done" {
		t.Fatalf("second claim: %q, want already_done", msg)
	}
}

// Spec test 21: an owner or admin of a suspended organizer can't start a new
// one; anyone else can.
func TestSuspendedTeamCantCreateOrganizer(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	var admin, checkin, stranger uuid.UUID
	if err := f.owner.QueryRow(ctx, `with a as (insert into users(email, name) values ('admin@example.com', 'A') returning id),
			c as (insert into users(email, name) values ('checkin@example.com', 'C') returning id),
			s as (insert into users(email, name) values ('stranger@example.com', 'S') returning id),
			m as (insert into memberships(organizer_id, user_id, role) select $1::uuid, id, 'admin' from a union all select $1::uuid, id, 'checkin' from c)
		select (select id from a), (select id from c), (select id from s)`, f.oa).Scan(&admin, &checkin, &stranger); err != nil {
		t.Fatal(err)
	}
	create := func(user uuid.UUID, slug string) error {
		return db.WithUser(ctx, f.pool, user, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select create_organizer('New', $1)`, slug)
			return err
		})
	}
	f.act(t, "suspend", uuid.NullUUID{})
	for name, u := range map[string]uuid.UUID{"owner": f.ua, "admin": admin} {
		if code, msg := pgCode(create(u, "new-"+name)); code != "42501" || msg != "suspended_member" {
			t.Errorf("%s of a suspended organizer: %s %q, want 42501 suspended_member", name, code, msg)
		}
	}
	for name, u := range map[string]uuid.UUID{"checkin": checkin, "stranger": stranger} {
		if err := create(u, "new-"+name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.act(t, "lift", uuid.NullUUID{})
	if err := create(admin, "new-admin"); err != nil {
		t.Errorf("admin after lift: %v", err)
	}
}

// Spec test 22: a takedown keeps its evidence. Neither the organizer nor an
// event a staff action names can be deleted, even by the schema owner. C has
// no sales, so only the takedown's rows stand in the way.
func TestTakedownKeepsEvidence(t *testing.T) {
	f := newStaffFixture(t)
	ctx := context.Background()
	_, oc := seedOrg(t, f.pool, "c@example.com", "org-c")
	var bare uuid.UUID
	if err := f.owner.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
		values ($1, 'bare-show', 'Bare', 'UTC', now(), now() + interval '1 hour') returning id`, oc).Scan(&bare); err != nil {
		t.Fatal(err)
	}
	login := seedLogin(t, f.owner, f.staff)
	sess, err := startStaff(ctx, f.pool, f.staff, login, "org-c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staffAct(ctx, f.pool, f.staff, sess, login, "suspend", uuid.NullUUID{}, takedownReason); err != nil {
		t.Fatal(err)
	}
	if _, err := staffAct(ctx, f.pool, f.staff, sess, login, "refund_event", uuid.NullUUID{UUID: bare, Valid: true}, takedownReason); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q   string
		arg uuid.UUID
	}{{`delete from events where id = $1`, bare}, {`delete from organizers where id = $1`, oc}} {
		_, err := f.owner.Exec(ctx, c.q, c.arg)
		if code, _ := pgCode(err); code != "23001" {
			t.Errorf("%s: %v, want 23001 restrict_violation", c.q, err)
		}
	}
}
