package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/staff"
)

const takedownReason = "Selling tickets to an illegal event"

func actBody(challenge, credential, kind, reason string) string {
	if credential == "" {
		credential = "{}"
	}
	r, _ := json.Marshal(reason)
	return `{"challenge_id":"` + challenge + `","credential":` + credential + `,"kind":"` + kind + `","reason":` + string(r) + `}`
}

// act takes a staff action on tok's staff-mode organizer with a fresh touch.
func (w staffWorld) act(tok, kind string) map[string]any {
	w.t.Helper()
	ch := w.must(w.do(tok, "POST", "/v1/staff/actions/challenge", `{"kind":"`+kind+`"}`), 200)
	res := w.do(tok, "POST", "/v1/staff/actions", actBody(ch["challenge_id"].(string), w.dev.assert(w.t, ch["options"]), kind, takedownReason))
	b := w.json(res)
	b["status"] = res.StatusCode
	return b
}

func (w staffWorld) orgID(slug string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.QueryRow(context.Background(), `select id from organizers where slug = $1`, slug).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// Test 26: a takedown needs a passkey touch made for exactly this action,
// on this organizer, in this login session.
func TestStaffActionAssertionIsBound(t *testing.T) {
	ctx := context.Background()
	w := newStaffWorld(t)
	uid := uuid.MustParse(w.must(w.do(w.staffTok, "GET", "/v1/me", ""), 200)["id"].(string))
	acme := w.orgID("acme")
	login := loginSession(t, w.owner, w.staffTok)
	refused := func(name, tok, body string) {
		t.Helper()
		res := w.do(tok, "POST", "/v1/staff/actions", body)
		if b := w.json(res); res.StatusCode != 403 || errCode(b) != "passkey_required" {
			t.Errorf("%s: got %d %v, want 403 passkey_required", name, res.StatusCode, b)
		}
	}
	signed := func(login uuid.UUID, kind, purpose string) (string, string) {
		t.Helper()
		id, opts, err := w.auth.BeginStaffAssertion(ctx, uid, login, kind, purpose)
		if err != nil {
			t.Fatal(err)
		}
		return id.String(), w.dev.assert(t, opts)
	}

	refused("no assertion", w.staffTok, actBody(uuid.NewString(), "", "suspend", takedownReason))
	id, cred := signed(login, "staff", staff.Purpose("suspend", acme, nil))
	refused("a staff-mode assertion", w.staffTok, actBody(id, cred, "suspend", takedownReason))
	id, cred = signed(login, "staff_action", staff.Purpose("lift", acme, nil))
	refused("a lift assertion sent as suspend", w.staffTok, actBody(id, cred, "suspend", takedownReason))
	id, cred = signed(loginSession(t, w.owner, w.staffIdle), "staff_action", staff.Purpose("suspend", acme, nil))
	refused("another login session's assertion", w.staffTok, actBody(id, cred, "suspend", takedownReason))
	id, cred = signed(loginSession(t, w.owner, w.staffElsewhere), "staff_action", staff.Purpose("suspend", acme, nil))
	refused("staff mode on another organizer", w.staffElsewhere, actBody(id, cred, "suspend", takedownReason))

	id, cred = signed(loginSession(t, w.owner, w.staffIdle), "staff_action", staff.Purpose("suspend", acme, nil))
	for _, r := range []struct{ path, body string }{
		{"/v1/staff/actions/challenge", `{"kind":"suspend"}`},
		{"/v1/staff/actions", actBody(id, cred, "suspend", takedownReason)},
	} {
		if res := w.do(w.staffIdle, "POST", r.path, r.body); res.StatusCode != 404 {
			t.Errorf("outside staff mode: %s got %d, want 404", r.path, res.StatusCode)
		}
	}
	if n := countRows(t, w.owner, `select count(*) from staff_actions`); n != 0 {
		t.Fatalf("staff_actions after refusals = %d", n)
	}
	if b := w.must(w.do(w.ownerA, "GET", "/v1/organizers/acme", ""), 200); b["suspended"] != false {
		t.Fatalf("acme after refusals: %v", b)
	}
}

// Test 27, and the refund_event refusal until the refund itself lands.
func TestSuspendAndLift(t *testing.T) {
	w := newStaffWorld(t)
	alerts := func(subject string) int {
		t.Helper()
		return countRows(t, w.owner, `select count(*) from jobs where kind = 'send_email' and payload->>'to' = 'alerts@example.com' and payload->>'subject' = $1`, subject)
	}

	b := w.act(w.staffTok, "suspend")
	if b["status"] != 200 || b["kind"] != "suspend" || b["organizer_slug"] != "acme" || b["id"] == nil {
		t.Fatalf("suspend: %v", b)
	}
	if n := countRows(t, w.owner, `select count(*) from staff_actions where kind = 'suspend' and reason = $1`, takedownReason); n != 1 {
		t.Fatalf("staff_actions = %d", n)
	}
	if alerts("Staff suspended acme") != 1 {
		t.Fatal("no suspend alert")
	}
	var text string
	if err := w.owner.QueryRow(context.Background(), `select payload->>'text' from jobs where payload->>'subject' = 'Staff suspended acme'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"staff@example.com", takedownReason, "UTC", "Report the account to Stripe support", "can't be undone"} {
		if !strings.Contains(text, want) {
			t.Errorf("the alert doesn't say %q: %s", want, text)
		}
	}
	var audit []map[string]any
	if err := json.NewDecoder(w.do(w.ownerA, "GET", "/v1/organizers/acme/audit", "").Body).Decode(&audit); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, r := range audit {
		if r["action"] == "staff.suspended" {
			found++
			if r["actor_type"] != "staff" || r["actor_email"] != nil {
				t.Errorf("audit row: %v", r)
			}
		}
	}
	if found != 1 {
		t.Fatalf("staff.suspended rows the owner reads: %d", found)
	}
	if o := w.must(w.do(w.ownerA, "GET", "/v1/organizers/acme", ""), 200); o["suspended"] != true {
		t.Fatalf("acme: %v", o)
	}
	var list []map[string]any
	if err := json.NewDecoder(w.do(w.ownerA, "GET", "/v1/organizers", "").Body).Decode(&list); err != nil || len(list) != 1 || list[0]["suspended"] != true {
		t.Fatalf("list: %v %v", list, err)
	}

	if b := w.act(w.staffTok, "suspend"); b["status"] != 409 || errCode(b) != "already_suspended" {
		t.Fatalf("suspend again: %v", b)
	}
	// Refunds aren't wired up yet: refused before anything is written.
	before := countRows(t, w.owner, `select count(*) from jobs`)
	res := w.do(w.staffTok, "POST", "/v1/staff/actions", strings.Replace(actBody(uuid.NewString(), "", "refund_event", takedownReason), `"kind"`, `"event":"`+w.fixture["event"]+`","kind"`, 1))
	if e, _ := w.json(res)["error"].(map[string]any); res.StatusCode != 422 || e["field"] != "kind" {
		t.Fatalf("refund_event: %d %v", res.StatusCode, e)
	}
	if after := countRows(t, w.owner, `select count(*) from jobs`); after != before || countRows(t, w.owner, `select count(*) from staff_actions`) != 1 {
		t.Fatal("refund_event wrote something")
	}

	if b := w.act(w.staffTok, "lift"); b["status"] != 200 || b["kind"] != "lift" {
		t.Fatalf("lift: %v", b)
	}
	if alerts("Staff lifted the suspension of acme") != 1 {
		t.Fatal("no lift alert")
	}
	if o := w.must(w.do(w.ownerA, "GET", "/v1/organizers/acme", ""), 200); o["suspended"] != false {
		t.Fatalf("acme after lift: %v", o)
	}
	if b := w.act(w.staffTok, "lift"); b["status"] != 409 || errCode(b) != "not_suspended" {
		t.Fatalf("lift again: %v", b)
	}

	var sessions []map[string]any
	if err := json.NewDecoder(w.do(w.staffTok, "GET", "/v1/staff/sessions", "").Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		acts, _ := s["actions"].([]any)
		if s["id"] == w.session && (len(acts) != 2 || acts[0].(map[string]any)["kind"] != "suspend" || acts[1].(map[string]any)["kind"] != "lift") {
			t.Errorf("staff session log: %v", s)
		}
	}
	// Organizers see the actions in their audit log, never in staff access.
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(w.do(w.ownerA, "GET", "/v1/organizers/acme/staff-access", "").Body)
	if strings.Contains(raw.String(), "actions") {
		t.Errorf("staff access shows actions: %s", raw.String())
	}
}
