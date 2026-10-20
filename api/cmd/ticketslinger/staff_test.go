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
	// staffEnv hands runStaff nothing but TS_MIGRATE_DATABASE_URL and SUDO_USER.
	getenv, owner := staffEnv(t, "james")
	mkUser(t, owner, "s@example.com", true, true)
	if _, err := runStaffCmd(getenv, "grant", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runStaffCmd(func(string) string { return "" }, "list"); err == nil || !strings.Contains(err.Error(), "TS_MIGRATE_DATABASE_URL") {
		t.Errorf("no URL: err = %v", err)
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
	if strings.Count(lines[0], "james") != 1 || len(strings.Fields(lines[1])) <= len(strings.Fields(lines[0])) {
		t.Errorf("revoked line should carry a revoked_at: %q", out)
	}
}
