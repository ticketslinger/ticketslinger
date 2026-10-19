package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/apikeys"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

const membersPath = "/v1/organizers/acme/members"

func (s shop) userID(email string) string {
	s.t.Helper()
	var id string
	if err := testdb.Owner(s.t, s.pool).QueryRow(context.Background(), `select id::text from users where email = $1`, email).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func TestOwnerAddsAndRemovesStaff(t *testing.T) {
	s := newShop(t, nil)
	_, qrs := s.doorTickets()
	door := s.signupWithMFA("door@example.com")
	if res := s.do(door, "GET", "/v1/organizers/acme", ""); res.StatusCode != 404 {
		t.Fatalf("before: %d, want 404", res.StatusCode)
	}
	m := s.must(s.do(s.owner, "POST", membersPath, `{"email":"door@example.com","role":"checkin"}`), 201)
	if m["email"] != "door@example.com" || m["role"] != "checkin" || m["user_id"] != s.userID("door@example.com") {
		t.Fatalf("added: %v", m)
	}
	s.must(s.do(door, "GET", "/v1/organizers/acme", ""), 200)
	if r := s.must(s.do(door, "POST", s.checkinPath(""), `{"qr":"`+qrs[0]+`"}`), 200); r["result"] != "accepted" {
		t.Fatalf("door scan: %v", r)
	}
	list := s.jsonList(s.do(s.owner, "GET", membersPath, ""))
	if len(list) != 2 {
		t.Fatalf("members: %v", list)
	}
	if res := s.do(s.owner, "DELETE", membersPath+"/"+s.userID("door@example.com"), ""); res.StatusCode != 204 {
		t.Fatalf("remove: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(door, "GET", "/v1/organizers/acme", ""); res.StatusCode != 404 {
		t.Fatalf("after: %d, want 404", res.StatusCode)
	}
	if res := s.do(s.owner, "DELETE", membersPath+"/"+s.userID("door@example.com"), ""); res.StatusCode != 404 {
		t.Fatalf("remove again: %d, want 404", res.StatusCode)
	}
}

func TestOnlyOwnersChangeTheTeam(t *testing.T) {
	s := newShop(t, nil)
	admin := s.member("admin@example.com", "admin")
	door := s.member("door@example.com", "checkin")
	s.signupVerified("new@example.com")
	expect := func(who, method, path, body string, status int, code string) {
		t.Helper()
		res := s.do(who, method, path, body)
		if got := s.errorCode(res); res.StatusCode != status || got != code {
			t.Fatalf("%s %s %s: got %d %q, want %d %q", method, path, body, res.StatusCode, got, status, code)
		}
	}
	add := `{"email":"new@example.com","role":"checkin"}`
	expect(admin, "POST", membersPath, add, 403, "forbidden_role")
	expect(admin, "DELETE", membersPath+"/"+s.userID("door@example.com"), "", 403, "forbidden_role")
	expect(door, "POST", membersPath, add, 403, "forbidden_role")
	expect(door, "GET", membersPath, "", 403, "forbidden_role")
	if list := s.jsonList(s.do(admin, "GET", membersPath, "")); len(list) != 3 {
		t.Fatalf("admin's list: %v", list)
	}

	res := s.do(s.owner, "POST", membersPath, `{"email":"new@example.com","role":"owner"}`)
	if e, _ := s.json(res)["error"].(map[string]any); res.StatusCode != 422 || e["field"] != "role" {
		t.Fatalf("role owner: %d %v, want 422 field role", res.StatusCode, e)
	}
	expect(s.owner, "POST", membersPath, `{"email":"not an email","role":"admin"}`, 422, "invalid_email")

	s.member("second@example.com", "owner")
	expect(s.owner, "DELETE", membersPath+"/"+s.userID("second@example.com"), "", 409, "cannot_remove")
	expect(s.owner, "DELETE", membersPath+"/"+s.userID("owner@example.com"), "", 409, "cannot_remove")

	_, key := s.createKey(s.owner, "acme", "write")
	expect(key, "GET", membersPath, "", 403, "session_required")
	expect(key, "POST", membersPath, add, 403, "session_required")
	expect(key, "DELETE", membersPath+"/"+s.userID("door@example.com"), "", 403, "session_required")

	// No one was added or removed by any of that.
	if n := s.count(`select count(*) from memberships m join organizers o on o.id = m.organizer_id where o.slug = 'acme'`); n != 4 {
		t.Fatalf("memberships: %d, want 4", n)
	}
}

// An unknown email and an unverified account get the same answer, so the
// page tells an owner no more than "not yet".
func TestAddMemberNeedsAVerifiedAccount(t *testing.T) {
	s := newShop(t, nil)
	s.signupUnverified("pending@example.com")
	bodies := map[string]string{}
	for _, email := range []string{"nobody@example.com", "pending@example.com"} {
		res := s.do(s.owner, "POST", membersPath, `{"email":"`+email+`","role":"admin"}`)
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 404 {
			t.Fatalf("%s: %d %s, want 404", email, res.StatusCode, b)
		}
		bodies[email] = string(b)
	}
	if bodies["nobody@example.com"] != bodies["pending@example.com"] {
		t.Fatalf("bodies differ: %v", bodies)
	}
	want := `{"error":{"code":"no_account","message":"There's no TicketSlinger account with that email yet. Ask them to sign up at app.ticketslinger.com, then add them here."}}`
	if got := bodies["nobody@example.com"]; got != want+"\n" && got != want {
		t.Fatalf("no_account body: %s", got)
	}

	s.signupVerified("ready@example.com")
	s.must(s.do(s.owner, "POST", membersPath, `{"email":"READY@example.com","role":"admin"}`), 201)
	for _, email := range []string{"ready@example.com", "owner@example.com"} {
		res := s.do(s.owner, "POST", membersPath, `{"email":"`+email+`","role":"checkin"}`)
		if res.StatusCode != 409 || s.errorCode(res) != "already_member" {
			t.Fatalf("%s again: %d %q, want 409 already_member", email, res.StatusCode, s.errorCode(res))
		}
	}
	if n := s.count(`select count(*) from memberships m join users u on u.id = m.user_id where u.email = 'ready@example.com' and m.role = 'admin'`); n != 1 {
		t.Fatalf("ready's membership changed: %d", n)
	}
}

func TestTeamChangesAreAudited(t *testing.T) {
	s := newShop(t, nil)
	s.signupVerified("door@example.com")
	s.must(s.do(s.owner, "POST", membersPath, `{"email":"door@example.com","role":"checkin"}`), 201)
	if res := s.do(s.owner, "DELETE", membersPath+"/"+s.userID("door@example.com"), ""); res.StatusCode != 204 {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	for _, q := range []string{
		`select count(*) from audit_log where action = 'member.added' and entity_type = 'member' and entity_id = $1
			and after = '{"email":"door@example.com","role":"checkin"}' and before is null and actor_user_id is not null`,
		`select count(*) from audit_log where action = 'member.removed' and entity_type = 'member' and entity_id = $1
			and before = '{"email":"door@example.com","role":"checkin"}' and after is null and actor_user_id is not null`,
	} {
		if n := s.count(q, s.userID("door@example.com")); n != 1 {
			t.Fatalf("%s: %d rows, want 1", q, n)
		}
	}
}

// A key carries its own authority, so a removed member's keys go with them;
// everyone else's keep working.
func TestRemovingAMemberRevokesTheirKeys(t *testing.T) {
	s := newShop(t, nil)
	admin := s.member("admin@example.com", "admin")
	adminKeyID, adminKey := s.createKey(admin, "acme", "write")
	_, ownerKey := s.createKey(s.owner, "acme", "read")
	s.must(s.do(adminKey, "GET", "/v1/organizers/acme", ""), 200)
	if res := s.do(s.owner, "DELETE", membersPath+"/"+s.userID("admin@example.com"), ""); res.StatusCode != 204 {
		t.Fatalf("remove: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(adminKey, "GET", "/v1/organizers/acme", ""); res.StatusCode != 401 {
		t.Fatalf("removed admin's key: %d, want 401", res.StatusCode)
	}
	s.must(s.do(ownerKey, "GET", "/v1/organizers/acme", ""), 200)
	if n := s.count(`select count(*) from audit_log where action = 'api_key.revoked' and entity_type = 'api_key' and entity_id = $1
		and before->>'scope' = 'write' and actor_user_id is not null`, adminKeyID); n != 1 {
		t.Fatalf("api_key.revoked rows: %d, want 1", n)
	}
}

// waitForLockWaiters returns once n backends of this database wait on a lock.
func (s shop) waitForLockWaiters(n int) {
	s.t.Helper()
	owner := testdb.Owner(s.t, s.pool)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := owner.QueryRow(context.Background(), `select count(distinct l.pid) from pg_locks l
			join pg_stat_activity a on a.pid = l.pid where not l.granted and a.datname = current_database()`).Scan(&waiting); err != nil {
			s.t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%d backends waiting on a lock after 10s, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// send runs a request off the test goroutine and reports its status.
func (s shop) sendAsync(token, method, path string) <-chan int {
	out := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(method, s.srv.URL+path, strings.NewReader(`{"name":"CI","scope":"write"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := s.client.Do(req)
		if err != nil {
			out <- 0
			return
		}
		_ = res.Body.Close()
		out <- res.StatusCode
	}()
	return out
}

func (s shop) orgID() uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	if err := testdb.Owner(s.t, s.pool).QueryRow(context.Background(), `select id from organizers where slug = 'acme'`).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// An admin making a key at the moment they're removed can't keep it: the
// removal waits for the key, then revokes it and logs that.
func TestKeyMadeDuringRemovalIsRevoked(t *testing.T) {
	s := newShop(t, nil)
	s.member("admin@example.com", "admin")
	admin := uuid.MustParse(s.userID("admin@example.com"))
	org := s.orgID()
	made, release := make(chan uuid.UUID), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	created := make(chan error, 1)
	go func() {
		created <- db.WithOrganizer(context.Background(), s.pool, admin, org, func(tx pgx.Tx, _ string) error {
			k, _, err := apikeys.New(s.pool).Create(context.Background(), tx, org, admin, "CI", "write")
			if err != nil {
				close(made)
				return err
			}
			made <- k.ID
			<-release
			return nil
		})
	}()
	key, ok := <-made
	if !ok {
		t.Fatal(<-created)
	}
	removed := s.sendAsync(s.owner, "DELETE", membersPath+"/"+admin.String())
	s.waitForLockWaiters(1)
	once.Do(func() { close(release) })
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if code := <-removed; code != 204 {
		t.Fatalf("remove: %d, want 204", code)
	}
	if n := s.count(`select count(*)::int from api_keys where id = $1 and revoked_at is not null`, key); n != 1 {
		t.Fatal("the key made during the removal still works")
	}
	if n := s.count(`select count(*)::int from audit_log where action = 'api_key.revoked' and entity_id = $1
		and before->>'scope' = 'write' and actor_user_id is not null`, key); n != 1 {
		t.Fatalf("api_key.revoked rows: %d, want 1", n)
	}
}

// The other way round: a key asked for while the removal is under way waits
// for it, then finds them gone.
func TestKeyAskedForDuringRemovalIsRefused(t *testing.T) {
	s := newShop(t, nil)
	adminTok := s.member("admin@example.com", "admin")
	admin := uuid.MustParse(s.userID("admin@example.com"))
	var owner uuid.UUID
	if err := testdb.Owner(t, s.pool).QueryRow(context.Background(), `select user_id from memberships where role = 'owner'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var r string
	if err := tx.QueryRow(ctx, `select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`,
		owner.String(), s.orgID().String()).Scan(&r, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select 1 from remove_member($1)`, admin); err != nil {
		t.Fatal(err)
	}
	asked := s.sendAsync(adminTok, "POST", "/v1/organizers/acme/api-keys")
	s.waitForLockWaiters(1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`select count(*)::int from memberships where user_id = $1`, admin); n != 0 {
		t.Fatal("remove_member left them on the team")
	}
	if code := <-asked; code != 404 {
		t.Fatalf("create key after removal: %d, want 404", code)
	}
	if n := s.count(`select count(*)::int from api_keys where created_by = $1 and revoked_at is null`, admin); n != 0 {
		t.Fatalf("%d live keys for a removed admin", n)
	}
}
