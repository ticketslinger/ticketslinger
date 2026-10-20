package staff_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/staff"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

const alertTo = "alerts@example.com"

type fixture struct {
	pool, owner *pgxpool.Pool
	svc         *staff.Service
	user, login uuid.UUID // a staff user and their MFA login session
	org         uuid.UUID // "acme", owned by someone else
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{pool: testdb.New(t)}
	f.owner = testdb.Owner(t, f.pool)
	f.svc = staff.New(f.pool, alertTo)
	var ann uuid.UUID
	if err := f.pool.QueryRow(ctx, `insert into users(email, name) values ('ann@example.com', 'Ann') returning id`).Scan(&ann); err != nil {
		t.Fatal(err)
	}
	o, err := organizers.New(f.pool).Create(ctx, ann, "Acme Events", "acme")
	if err != nil {
		t.Fatal(err)
	}
	f.org = o.ID
	if err := f.owner.QueryRow(ctx, `with u as (insert into users(email, name, email_verified_at) values ('s@example.com', 'Sam', now()) returning id)
		insert into staff(user_id, granted_by) select id, 'james' from u returning user_id`).Scan(&f.user); err != nil {
		t.Fatal(err)
	}
	f.login = f.newLogin(t)
	return f
}

func (f fixture) newLogin(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.owner.QueryRow(context.Background(), `insert into sessions(user_id, token_hash, mfa, expires_at)
		values ($1, sha256(gen_random_uuid()::text::bytea), true, now() + interval '1 hour') returning id`, f.user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartWritesSessionAuditAndAlert(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	sess, err := f.svc.Start(ctx, f.user, f.login, "acme", "Buyer says the ticket email never came", "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.OrganizerID != f.org || sess.OrganizerSlug != "acme" || sess.OrganizerName != "Acme Events" || sess.LoginSession != f.login {
		t.Fatalf("session: %+v", sess)
	}
	if n := f.count(t, `select count(*) from staff_sessions where session_id = $1 and ended_at is null`, f.login); n != 1 {
		t.Fatalf("staff_sessions rows = %d", n)
	}
	if n := f.count(t, `select count(*) from audit_log where organizer_id = $1 and action = 'staff.session_started'
		and actor_staff_session_id = $2 and after->>'reason' = 'Buyer says the ticket email never came'`, f.org, sess.ID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	var payload []byte
	if err := f.owner.QueryRow(ctx, `select payload from jobs where kind = 'send_email'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var mail struct{ To, Subject, Text string }
	_ = json.Unmarshal(payload, &mail)
	if mail.To != alertTo || mail.Subject != "Staff mode started on acme" ||
		!strings.Contains(mail.Text, "s@example.com") || !strings.Contains(mail.Text, "Buyer says the ticket email never came") || !strings.Contains(mail.Text, "UTC") {
		t.Fatalf("alert: %+v", mail)
	}

	cur, err := f.svc.Current(ctx, f.user, f.login)
	if err != nil || cur == nil || cur.ID != sess.ID || cur.OrganizerName != "Acme Events" {
		t.Fatalf("current: %+v %v", cur, err)
	}
	if cur, err := f.svc.Current(ctx, f.user, f.newLogin(t)); err != nil || cur != nil {
		t.Fatalf("another login session's current: %+v %v", cur, err)
	}
	if err := f.svc.End(ctx, f.user, f.login, "staff", "Sorted"); err != nil {
		t.Fatal(err)
	}
	if cur, err := f.svc.Current(ctx, f.user, f.login); err != nil || cur != nil {
		t.Fatalf("current after end: %+v %v", cur, err)
	}
	if err := f.svc.End(ctx, f.user, f.login, "staff", ""); !errors.Is(err, staff.ErrNoSession) {
		t.Fatalf("ending twice: %v", err)
	}
}

func TestStartRefusedWithoutAlertAddress(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := staff.New(f.pool, "").Start(ctx, f.user, f.login, "acme", "Buyer says the ticket email never came", "", ""); !errors.Is(err, staff.ErrAlertsOff) {
		t.Fatalf("got %v, want ErrAlertsOff", err)
	}
	if n := f.count(t, `select count(*) from staff_sessions`); n != 0 {
		t.Fatalf("staff_sessions rows = %d", n)
	}
}

func TestStartRefusesNonStaffAndUnknownOrganizers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.svc.Start(ctx, f.user, f.login, "nope", "Buyer says the ticket email never came", "", ""); !errors.Is(err, staff.ErrNoOrganizer) {
		t.Fatalf("unknown organizer: %v", err)
	}
	if _, err := f.owner.Exec(ctx, `update staff set revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Start(ctx, f.user, f.login, "acme", "Buyer says the ticket email never came", "", ""); !errors.Is(err, staff.ErrNotAllowed) {
		t.Fatalf("revoked staff: %v", err)
	}
	if ok, err := f.svc.IsStaff(ctx, f.user); err != nil || ok {
		t.Fatalf("revoked IsStaff: %v %v", ok, err)
	}
}

func TestStartEndsThePreviousSession(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	first, err := f.svc.Start(ctx, f.user, f.login, "acme", "First look at the refund", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Start(ctx, f.user, f.login, "acme", "Second look at the refund", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var how string
	if err := f.owner.QueryRow(ctx, `select ended_how from staff_sessions where id = $1`, first.ID).Scan(&how); err != nil || how != "staff" {
		t.Fatalf("first ended_how = %q %v", how, err)
	}
	if cur, err := f.svc.Current(ctx, f.user, f.login); err != nil || cur == nil || cur.ID != second.ID {
		t.Fatalf("current: %+v %v", cur, err)
	}
	if _, err := f.owner.Exec(ctx, `insert into staff_access_log(staff_session_id, organizer_id, method, route) values ($1, $2, 'GET', 'getOrganizer')`, second.ID, f.org); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.List(ctx, f.user)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[0].StaffName != "Sam" || len(list[1].Requests) != 0 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if r := list[0].Requests; len(r) != 1 || r[0].Method != "GET" || r[0].Route != "getOrganizer" || time.Since(r[0].At) > time.Minute {
		t.Fatalf("requests: %+v", r)
	}
}

func TestSweepClosesExpired(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var id uuid.UUID
	if err := f.owner.QueryRow(ctx, `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, started_at, expires_at)
		values ($1, $2, $3, 'acme', 'Acme Events', 'An hour ago, nobody ended it', now() - interval '2 hours', now() - interval '1 hour') returning id`,
		f.user, f.login, f.org).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Sweep(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `select count(*) from staff_sessions where id = $1 and ended_how = 'expired'`, id); n != 1 {
		t.Fatal("the sweep left an expired staff session open")
	}
}

func TestSearchEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, q := range []string{"%", "_", `\`, "a%e", "a_m"} {
		got, err := f.svc.Search(ctx, f.user, q)
		if err != nil || len(got) != 0 {
			t.Errorf("search %q: %+v %v, want nothing", q, got, err)
		}
	}
	got, err := f.svc.Search(ctx, f.user, "ACME ev")
	if err != nil || len(got) != 1 || got[0] != (staff.OrgRef{Name: "Acme Events", Slug: "acme"}) {
		t.Fatalf("search by name: %+v %v", got, err)
	}
}
