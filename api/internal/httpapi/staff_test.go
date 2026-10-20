package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// staffOps is every /staff/* operation, with a body that would otherwise pass.
var staffOps = []struct{ method, path, body string }{
	{"POST", "/v1/staff/sessions/challenge", ""},
	{"POST", "/v1/staff/sessions", `{"challenge_id":"0190a0a0-0000-7000-8000-000000000000","credential":{},"organizer":"acme","reason":"Buyer says the ticket email never came"}`},
	{"DELETE", "/v1/staff/sessions/current", `{}`},
	{"GET", "/v1/staff/organizers?q=acme", ""},
	{"GET", "/v1/staff/sessions", ""},
	{"POST", "/v1/staff/actions/challenge", `{"kind":"suspend"}`},
	{"POST", "/v1/staff/actions", `{"challenge_id":"0190a0a0-0000-7000-8000-000000000000","credential":{},"kind":"suspend","reason":"Selling tickets to an illegal event"}`},
}

type staffHarness struct {
	*harness
	auth  *auth.Service
	owner *pgxpool.Pool
}

// newStaffHarness is newHarness with the auth service and an owner pool at
// hand, and staff alerts going to alertTo ("" for none).
func newStaffHarness(t *testing.T, alertTo string) staffHarness {
	pool := testdb.New(t)
	m := &mailer.Memory{}
	svc := auth.NewService(pool, m, auth.Options{RPID: "localhost", Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	srv := startServer(t, httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Config: config.Config{MailAlertTo: alertTo}}))
	return staffHarness{&harness{t, srv, m, pool, clientFor(t, srv.URL)}, svc, testdb.Owner(t, pool)}
}

// grantStaff makes the account with email staff, as the CLI would.
func grantStaff(t *testing.T, owner *pgxpool.Pool, email string) {
	t.Helper()
	if _, err := owner.Exec(context.Background(), `insert into staff(user_id, granted_by) select id, 'james' from users where email = $1`, email); err != nil {
		t.Fatal(err)
	}
}

// loginSession is the sessions.id behind a session token.
func loginSession(t *testing.T, owner *pgxpool.Pool, token string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := owner.QueryRow(context.Background(), `select id from sessions where token_hash = sha256(convert_to($1, 'UTF8'))`, token).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

var staffRP = virtualwebauthn.RelyingParty{Name: "TicketSlinger", ID: "localhost", Origin: "http://localhost:5173"}

type passkeyDevice struct {
	authr virtualwebauthn.Authenticator
	cred  virtualwebauthn.Credential
}

// assert answers WebAuthn request options the way navigator.credentials.get would.
func (d *passkeyDevice) assert(t *testing.T, options any) string {
	t.Helper()
	b, _ := json.Marshal(options)
	o, err := virtualwebauthn.ParseAssertionOptions(string(b))
	if err != nil {
		t.Fatal(err)
	}
	d.cred.Counter++
	return virtualwebauthn.CreateAssertionResponse(staffRP, d.authr, d.cred, *o)
}

// signupWithPasskey makes a verified account with one passkey and returns
// the device and a passkey (mfa) session token.
func (h staffHarness) signupWithPasskey(email string) (*passkeyDevice, string) {
	h.t.Helper()
	tok := h.signupVerified(email)
	me := h.must(h.do(tok, "GET", "/v1/me", ""), 200)
	uid := uuid.MustParse(me["id"].(string))
	d := &passkeyDevice{virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: uid[:]}), virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)}
	ch := h.must(h.do(tok, "POST", "/v1/auth/passkeys/register", `{"password":"`+testPassword+`"}`), 200)
	b, _ := json.Marshal(ch["options"])
	att, err := virtualwebauthn.ParseAttestationOptions(string(b))
	if err != nil {
		h.t.Fatal(err)
	}
	h.must(h.do(tok, "POST", "/v1/auth/passkeys/register/"+ch["challenge_id"].(string), virtualwebauthn.CreateAttestationResponse(staffRP, d.authr, d.cred, *att)), 201)
	return d, h.passkeyLogin(d)
}

func (h staffHarness) passkeyLogin(d *passkeyDevice) string {
	h.t.Helper()
	ch := h.must(h.do("", "POST", "/v1/auth/passkeys/login", ""), 200)
	s := h.must(h.do("", "POST", "/v1/auth/passkeys/login/"+ch["challenge_id"].(string), d.assert(h.t, ch["options"])), 200)
	return s["session_token"].(string)
}

func startBody(challenge string, credential string, reason string) string {
	if credential == "" {
		credential = "{}"
	}
	r, _ := json.Marshal(reason)
	return `{"challenge_id":"` + challenge + `","credential":` + credential + `,"organizer":"acme","reason":` + string(r) + `}`
}

const goodReason = "Buyer says the ticket email never came"

func TestStaffRoutesAre404ToOthers(t *testing.T) {
	h := newStaffHarness(t, "alerts@example.com")
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	_, readKey := h.createKey(owner, "acme", "read")
	_, writeKey := h.createKey(owner, "acme", "write")
	// Staff, but signed in with a password only: an ordinary user.
	plainStaff := h.signupVerified("plain@example.com")
	grantStaff(t, h.owner, "plain@example.com")
	// What an unknown route answers (TestUnknownRouteIsJSONError).
	unknown := map[string]any{"error": map[string]any{"code": "not_found", "message": "That doesn't exist, or you don't have access to it."}}

	for name, tok := range map[string]string{"non-staff": owner, "non-mfa staff": plainStaff, "read key": readKey, "write key": writeKey} {
		for _, op := range staffOps {
			// A body that can't be read gets the same 404: the gate runs first.
			for _, body := range []string{op.body, `{"challenge_id":"not a uuid"`} {
				res := h.do(tok, op.method, op.path, body)
				b := h.json(res)
				if res.StatusCode != 404 || !jsonEqual(b, unknown) {
					t.Errorf("%s: %s %s %.30q got %d %v, want 404 %v", name, op.method, op.path, body, res.StatusCode, b, unknown)
				}
			}
		}
	}
	if n := countRows(t, h.owner, `select count(*) from webauthn_challenges where kind = 'staff'`); n != 0 {
		t.Fatalf("staff challenges = %d", n)
	}
	me := h.must(h.do(plainStaff, "GET", "/v1/me", ""), 200)
	if me["staff"] != false || me["staff_session"] != nil {
		t.Fatalf("a non-mfa staff session's me: %v", me)
	}
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartNeedsAStaffPasskeyTouch(t *testing.T) {
	ctx := context.Background()
	h := newStaffHarness(t, "alerts@example.com")
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	dev, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	other := h.passkeyLogin(dev) // the same person's second login session

	me := h.must(h.do(tok, "GET", "/v1/me", ""), 200)
	if me["staff"] != true || me["staff_session"] != nil {
		t.Fatalf("me before: %v", me)
	}
	refused := func(name, body string) {
		t.Helper()
		res := h.do(tok, "POST", "/v1/staff/sessions", body)
		if code := h.errorCode(res); res.StatusCode != 403 || code != "passkey_required" {
			t.Errorf("%s: got %d %q, want 403 passkey_required", name, res.StatusCode, code)
		}
	}
	refused("no assertion", startBody(uuid.NewString(), "", goodReason))

	ch := h.must(h.do("", "POST", "/v1/auth/passkeys/login", ""), 200)
	refused("a sign-in assertion", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason))

	ch = h.must(h.do(other, "POST", "/v1/staff/sessions/challenge", ""), 200)
	refused("another login session's assertion", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason))

	uid := uuid.MustParse(me["id"].(string))
	login := loginSession(t, h.owner, tok)
	id, opts, err := h.auth.BeginStaffAssertion(ctx, uid, login, "staff_action", "start")
	if err != nil {
		t.Fatal(err)
	}
	refused("a staff_action assertion", startBody(id.String(), dev.assert(t, opts), goodReason))
	id, opts, err = h.auth.BeginStaffAssertion(ctx, uid, login, "staff", "suspend:acme")
	if err != nil {
		t.Fatal(err)
	}
	refused("a staff assertion for another purpose", startBody(id.String(), dev.assert(t, opts), goodReason))

	ch = h.must(h.do(tok, "POST", "/v1/staff/sessions/challenge", ""), 200)
	if _, err := h.owner.Exec(ctx, `update webauthn_challenges set expires_at = now() - interval '1 second' where id = $1`, ch["challenge_id"]); err != nil {
		t.Fatal(err)
	}
	refused("an expired challenge", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason))

	ch = h.must(h.do(tok, "POST", "/v1/staff/sessions/challenge", ""), 200)
	dev.authr.Options.UserNotVerified = true
	refused("a touch without user verification", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason))
	dev.authr.Options.UserNotVerified = false

	if n := countRows(t, h.owner, `select count(*) from staff_sessions`); n != 0 {
		t.Fatalf("staff_sessions after refusals = %d", n)
	}

	ch = h.must(h.do(tok, "POST", "/v1/staff/sessions/challenge", ""), 200)
	body := startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason)
	started := h.must(h.do(tok, "POST", "/v1/staff/sessions", body), 201)
	if started["organizer_slug"] != "acme" || started["organizer_name"] != "acme" || started["reason"] != goodReason {
		t.Fatalf("started: %v", started)
	}
	// Signed afresh with a higher counter, so only the challenge being
	// spent can refuse it (synced passkeys report a counter of 0).
	refused("a reused challenge", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason))

	me = h.must(h.do(tok, "GET", "/v1/me", ""), 200)
	cur, _ := me["staff_session"].(map[string]any)
	if cur == nil || cur["id"] != started["id"] || cur["organizer_slug"] != "acme" {
		t.Fatalf("me during staff mode: %v", me)
	}
	if me := h.must(h.do(other, "GET", "/v1/me", ""), 200); me["staff_session"] != nil {
		t.Fatalf("the other login session is in staff mode too: %v", me)
	}

	var orgs []map[string]any
	_ = json.NewDecoder(h.do(tok, "GET", "/v1/staff/organizers?q="+url.QueryEscape("acm"), "").Body).Decode(&orgs)
	if len(orgs) != 1 || orgs[0]["slug"] != "acme" {
		t.Fatalf("search: %v", orgs)
	}
	if res := h.do(tok, "GET", "/v1/staff/organizers?q=a", ""); res.StatusCode != 422 || h.errorCode(res) == "" {
		t.Fatalf("one-letter search: %d", res.StatusCode)
	}
	var list []map[string]any
	res := h.do(tok, "GET", "/v1/staff/sessions", "")
	_ = json.NewDecoder(res.Body).Decode(&list)
	if res.StatusCode != 200 || len(list) != 1 || list[0]["reason"] != goodReason {
		t.Fatalf("list: %d %v", res.StatusCode, list)
	}

	h.must(h.do(tok, "DELETE", "/v1/staff/sessions/current", `{"note":"Resent the email"}`), 204)
	if me := h.must(h.do(tok, "GET", "/v1/me", ""), 200); me["staff_session"] != nil {
		t.Fatalf("me after end: %v", me)
	}
	if res := h.do(tok, "DELETE", "/v1/staff/sessions/current", `{}`); res.StatusCode != 404 {
		t.Fatalf("ending again: %d", res.StatusCode)
	}
	if n := countRows(t, h.owner, `select count(*) from staff_sessions where ended_how = 'staff' and end_note = 'Resent the email'`); n != 1 {
		t.Fatal("the end wasn't recorded with its note")
	}
}

func TestLogoutEndsStaffMode(t *testing.T) {
	h := newStaffHarness(t, "alerts@example.com")
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	dev, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	ch := h.must(h.do(tok, "POST", "/v1/staff/sessions/challenge", ""), 200)
	h.must(h.do(tok, "POST", "/v1/staff/sessions", startBody(ch["challenge_id"].(string), dev.assert(t, ch["options"]), goodReason)), 201)
	h.must(h.do(tok, "POST", "/v1/auth/logout", ""), 204)
	if n := countRows(t, h.owner, `select count(*) from staff_sessions where ended_how = 'logout' and ended_at is not null`); n != 1 {
		t.Fatal("logout left staff mode running")
	}
}

func TestStartReasonBounds(t *testing.T) {
	h := newStaffHarness(t, "alerts@example.com")
	_, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	for _, n := range []int{9, 501} {
		res := h.do(tok, "POST", "/v1/staff/sessions", startBody(uuid.NewString(), "", strings.Repeat("r", n)))
		b := h.json(res)
		e, _ := b["error"].(map[string]any)
		if res.StatusCode != http.StatusUnprocessableEntity || e["field"] != "reason" {
			t.Errorf("reason of %d: got %d %v, want 422 field reason", n, res.StatusCode, b)
		}
	}
}

// With no alert address, staff mode doesn't start, and a touch already
// made isn't spent.
func TestStaffModeNeedsAnAlertAddress(t *testing.T) {
	ctx := context.Background()
	h := newStaffHarness(t, "")
	h.createOrganizer(h.signupWithMFA("owner@example.com"), "acme")
	dev, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	if res := h.do(tok, "POST", "/v1/staff/sessions/challenge", ""); res.StatusCode != 503 || h.errorCode(res) != "alerts_off" {
		t.Fatalf("begin: %d %q", res.StatusCode, h.errorCode(res))
	}
	me := h.must(h.do(tok, "GET", "/v1/me", ""), 200)
	id, opts, err := h.auth.BeginStaffAssertion(ctx, uuid.MustParse(me["id"].(string)), loginSession(t, h.owner, tok), "staff", "start")
	if err != nil {
		t.Fatal(err)
	}
	if res := h.do(tok, "POST", "/v1/staff/sessions", startBody(id.String(), dev.assert(t, opts), goodReason)); res.StatusCode != 503 || h.errorCode(res) != "alerts_off" {
		t.Fatalf("start: %d %q", res.StatusCode, h.errorCode(res))
	}
	if n := countRows(t, h.owner, `select count(*) from webauthn_challenges where id = $1`, id); n != 1 {
		t.Fatal("the challenge was spent")
	}
}

// Final review Minor 3: a staff member gets 10 passkey challenges a minute
// and 10 answers, then 429 rate_limited.
func TestStaffPasskeyLimits(t *testing.T) {
	h := newStaffHarness(t, "alerts@example.com")
	h.createOrganizer(h.signupWithMFA("owner@example.com"), "acme")
	staffTok := func(email string) string {
		_, tok := h.signupWithPasskey(email)
		grantStaff(t, h.owner, email)
		return tok
	}
	tok := staffTok("staff@example.com")
	limited := func(name, path, body string, want int) {
		t.Helper()
		for i := 1; i <= 10; i++ {
			if res := h.do(tok, "POST", path, body); res.StatusCode != want {
				t.Fatalf("%s %d: got %d, want %d", name, i, res.StatusCode, want)
			}
		}
		res := h.do(tok, "POST", path, body)
		if got := h.json(res); res.StatusCode != 429 || errCode(got) != "rate_limited" {
			t.Fatalf("%s 11: got %d %v, want 429 rate_limited", name, res.StatusCode, got)
		}
	}
	limited("begin", "/v1/staff/sessions/challenge", "", 200)
	limited("verify", "/v1/staff/sessions", startBody(uuid.NewString(), "", goodReason), 403)
	// 20 answers a minute from one address, whoever sends them: the first
	// staff member's 11 count, so a second gets 9.
	second := staffTok("second@example.com")
	for i := 1; i <= 9; i++ {
		if res := h.do(second, "POST", "/v1/staff/sessions", startBody(uuid.NewString(), "", goodReason)); res.StatusCode != 403 {
			t.Fatalf("second staff member's answer %d: got %d, want 403", i, res.StatusCode)
		}
	}
	res := h.do(second, "POST", "/v1/staff/sessions", startBody(uuid.NewString(), "", goodReason))
	if got := h.json(res); res.StatusCode != 429 || errCode(got) != "rate_limited" {
		t.Fatalf("the address's 21st answer: got %d %v, want 429 rate_limited", res.StatusCode, got)
	}
}
