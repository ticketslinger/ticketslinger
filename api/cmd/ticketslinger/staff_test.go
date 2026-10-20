package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// staffEnv is an env with only the owner URL (and SUDO_USER), as the server's
// shell would have it, plus the owner pool for fixtures.
func staffEnv(t *testing.T, sudoUser string) (func(string) string, *pgxpool.Pool) {
	t.Helper()
	owner := testdb.Owner(t, testdb.New(t))
	env := map[string]string{"TS_MIGRATE_DATABASE_URL": owner.Config().ConnString(), "SUDO_USER": sudoUser}
	return func(k string) string { return env[k] }, owner
}

func runStaffCmd(getenv func(string) string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runStaff(context.Background(), getenv, args, &out)
	return out.String(), err
}

func mkUser(t *testing.T, owner *pgxpool.Pool, email string, verified, passkey bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	q := `insert into users(email, name, email_verified_at) values ($1, 'U', case when $2 then now() end) returning id`
	if err := owner.QueryRow(ctx, q, email, verified).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if passkey {
		if _, err := owner.Exec(ctx, `insert into passkeys(user_id, credential_id, credential, name) values ($1, $2, '{}', 'k')`, id, []byte(email)); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func staffCount(t *testing.T, owner *pgxpool.Pool) (n int) {
	t.Helper()
	if err := owner.QueryRow(context.Background(), `select count(*) from staff`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStaffGrantNeedsVerifiedPasskeyAccount(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	mkUser(t, owner, "unverified@example.com", false, true)
	mkUser(t, owner, "nokey@example.com", true, false)
	for email, want := range map[string]string{
		"nobody@example.com":     "no account with that email",
		"unverified@example.com": "that account's email isn't verified",
		"nokey@example.com":      "that account has no passkey: add one in Dashboard → Security, then grant again",
	} {
		_, err := runStaffCmd(getenv, "grant", email)
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", email, err, want)
		}
	}
	if n := staffCount(t, owner); n != 0 {
		t.Errorf("staff rows = %d, want 0", n)
	}
}

func TestStaffGrantRevokeRegrant(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	ctx := context.Background()
	user := mkUser(t, owner, "s@example.com", true, true)
	if out, err := runStaffCmd(getenv, "grant", "s@example.com"); err != nil || out != "granted s@example.com\n" {
		t.Fatalf("grant = %q, %v", out, err)
	}
	var by string
	if err := owner.QueryRow(ctx, `select granted_by from staff where user_id = $1`, user).Scan(&by); err != nil || by != "james" {
		t.Fatalf("granted_by = %q, %v", by, err)
	}
	var org, sess, ss uuid.UUID
	if err := owner.QueryRow(ctx, `insert into organizers(name, slug) values ('Acme', 'acme') returning id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `insert into sessions(user_id, token_hash, mfa, expires_at) values ($1, '\x01', true, now() + interval '1 day') returning id`, user).Scan(&sess); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, expires_at)
		values ($1, $2, $3, 'acme', 'Acme', 'investigating ticket 42', now() + interval '30 minutes') returning id`, user, sess, org).Scan(&ss); err != nil {
		t.Fatal(err)
	}
	out, err := runStaffCmd(getenv, "revoke", "s@example.com")
	if err != nil || out != "revoked s@example.com, ended 1 session(s)\n" {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	var how string
	if err := owner.QueryRow(ctx, `select ended_how from staff_sessions where id = $1 and ended_at is not null`, ss).Scan(&how); err != nil || how != "revoked" {
		t.Fatalf("ended_how = %q, %v", how, err)
	}
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from audit_log where action = 'staff.session_ended' and organizer_id = $1
		and actor_user_id = $2 and actor_staff_session_id = $3 and not actor_system and after->>'how' = 'revoked'`, org, user, ss).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows = %d, %v", n, err)
	}
	var revoked bool
	if err := owner.QueryRow(ctx, `select revoked_at is not null from staff where user_id = $1`, user).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("revoked_at set = %v, %v", revoked, err)
	}
	getenv2 := func(k string) string {
		if k == "SUDO_USER" {
			return "ana"
		}
		return getenv(k)
	}
	if _, err := runStaffCmd(getenv2, "grant", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `select revoked_at is not null, granted_by from staff where user_id = $1`, user).Scan(&revoked, &by); err != nil || revoked || by != "ana" {
		t.Fatalf("after regrant: revoked=%v by=%q err=%v", revoked, by, err)
	}
}

func TestStaffGrantNeedsSudoUser(t *testing.T) {
	getenv, owner := staffEnv(t, "")
	mkUser(t, owner, "s@example.com", true, true)
	_, err := runStaffCmd(getenv, "grant", "s@example.com")
	if err == nil || err.Error() != "run this through sudo ts-staff" {
		t.Fatalf("err = %v", err)
	}
	if n := staffCount(t, owner); n != 0 {
		t.Errorf("staff rows = %d, want 0", n)
	}
}

func TestStaffCommandUsesOnlyTheOwnerURL(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	mkUser(t, owner, "s@example.com", true, true)
	junk := func(k string) string {
		switch k {
		case "TS_DATABASE_URL":
			return "postgres://nobody:x@127.0.0.1:1/none"
		case "TS_SECRET_KEY", "TS_PUBLIC_URL", "TS_HTTP_ADDR", "TS_MAIL":
			return "!!junk!!"
		}
		return getenv(k)
	}
	if _, err := runStaffCmd(junk, "grant", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runStaffCmd(func(string) string { return "" }, "list"); err == nil || !strings.Contains(err.Error(), "TS_MIGRATE_DATABASE_URL") {
		t.Errorf("no URL: err = %v", err)
	}
}

func mkLive(t *testing.T, owner *pgxpool.Pool, user uuid.UUID, slug string, token byte, expires string) (ss uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var org, sess uuid.UUID
	if err := owner.QueryRow(ctx, `insert into organizers(name, slug) values ($1::text, $1::citext) returning id`, slug).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `insert into sessions(user_id, token_hash, mfa, expires_at) values ($1, $2, true, now() + interval '1 day') returning id`, user, []byte{token}).Scan(&sess); err != nil {
		t.Fatal(err)
	}
	// started_at is backdated so expires_at can be in the past within the 60 minute CHECK.
	if err := owner.QueryRow(ctx, `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, started_at, expires_at)
		values ($1, $2, $3, $4::citext, $4::text, 'investigating ticket 42', now() - interval '50 minutes', now() + $5::interval) returning id`, user, sess, org, slug, expires).Scan(&ss); err != nil {
		t.Fatal(err)
	}
	return ss
}

func TestStaffRevokeEndsLiveSessionsOnly(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	ctx := context.Background()
	user := mkUser(t, owner, "s@example.com", true, true)
	if _, err := runStaffCmd(getenv, "grant", "S@Example.com"); err != nil { // citext
		t.Fatal(err)
	}
	a := mkLive(t, owner, user, "org-a", 1, "5 minutes")
	b := mkLive(t, owner, user, "org-b", 2, "5 minutes")
	old := mkLive(t, owner, user, "org-c", 3, "5 minutes")
	lapsed := mkLive(t, owner, user, "org-d", 4, "-1 minute")
	if _, err := owner.Exec(ctx, `update staff_sessions set ended_at = now() - interval '1 minute', ended_how = 'staff' where id = $1`, old); err != nil {
		t.Fatal(err)
	}
	out, err := runStaffCmd(getenv, "revoke", "S@EXAMPLE.COM")
	if err != nil || out != "revoked S@EXAMPLE.COM, ended 2 session(s)\n" {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	for id, want := range map[uuid.UUID]string{a: "revoked", b: "revoked", old: "staff", lapsed: "expired"} {
		var how string
		if err := owner.QueryRow(ctx, `select ended_how from staff_sessions where id = $1`, id).Scan(&how); err != nil || how != want {
			t.Errorf("session %s ended_how = %q (%v), want %q", id, how, err, want)
		}
	}
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from audit_log where action = 'staff.session_ended' and after->>'how' = 'revoked'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("revoked audit rows = %d, %v; want 2", n, err)
	}
	// Revoking again is idempotent: nothing left to end.
	out, err = runStaffCmd(getenv, "revoke", "s@example.com")
	if err != nil || out != "revoked s@example.com, ended 0 session(s)\n" {
		t.Errorf("second revoke = %q, %v", out, err)
	}
	if _, err := runStaffCmd(getenv, "revoke", "nobody@example.com"); err == nil || err.Error() != "that account isn't staff" {
		t.Errorf("non-staff revoke: %v", err)
	}
}

func TestStaffRegrantEndsSessionsThatSlippedIn(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	ctx := context.Background()
	user := mkUser(t, owner, "s@example.com", true, true)
	if _, err := runStaffCmd(getenv, "grant", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runStaffCmd(getenv, "revoke", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	ss := mkLive(t, owner, user, "org-a", 1, "5 minutes")
	if _, err := runStaffCmd(getenv, "grant", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	var how string
	if err := owner.QueryRow(ctx, `select ended_how from staff_sessions where id = $1 and ended_at is not null`, ss).Scan(&how); err != nil || how != "revoked" {
		t.Fatalf("ended_how = %q, %v", how, err)
	}
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from audit_log where actor_staff_session_id = $1 and action = 'staff.session_ended'`, ss).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows = %d, %v", n, err)
	}
}

func TestStaffList(t *testing.T) {
	getenv, owner := staffEnv(t, "james")
	mkUser(t, owner, "b@example.com", true, true)
	mkUser(t, owner, "a@example.com", true, true)
	for _, e := range []string{"b@example.com", "a@example.com"} {
		if _, err := runStaffCmd(getenv, "grant", e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runStaffCmd(getenv, "revoke", "b@example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := runStaffCmd(getenv, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "a@example.com") || !strings.HasPrefix(lines[1], "b@example.com") {
		t.Fatalf("list = %q", out)
	}
	if len(strings.Fields(lines[0])) != 3 || len(strings.Fields(lines[1])) != 4 {
		t.Errorf("only the revoked line carries a revoked_at: %q", out)
	}
}
