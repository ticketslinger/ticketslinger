package httpapi_test

import (
	"context"
	"io"
	"testing"

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
