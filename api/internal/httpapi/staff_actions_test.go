package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// actOn takes a staff action naming event.
func (w staffWorld) actOn(tok, kind, event string) map[string]any {
	w.t.Helper()
	ch := w.must(w.do(tok, "POST", "/v1/staff/actions/challenge", `{"kind":"`+kind+`","event":"`+event+`"}`), 200)
	body := actBody(ch["challenge_id"].(string), w.dev.assert(w.t, ch["options"]), kind, takedownReason)
	res := w.do(tok, "POST", "/v1/staff/actions", strings.Replace(body, `"kind"`, `"event":"`+event+`","kind"`, 1))
	b := w.json(res)
	b["status"] = res.StatusCode
	return b
}

// refunded waits for the refund_event action id to finish, as the Staff
// page does, and returns its result.
func (w staffWorld) refunded(id string) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var sessions []map[string]any
		if err := json.NewDecoder(w.do(w.staffTok, "GET", "/v1/staff/sessions", "").Body).Decode(&sessions); err != nil {
			w.t.Fatal(err)
		}
		for _, s := range sessions {
			acts, _ := s["actions"].([]any)
			for _, a := range acts {
				if m := asMap(a); m["id"] == id && m["finished_at"] != nil {
					return asMap(m["result"])
				}
			}
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("refund %s didn't finish", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// Tests 27 and 28.
func TestSuspendAndLift(t *testing.T) {
	w := newStaffWorld(t)
	alerts := func(subject string) int {
		t.Helper()
		return countRows(t, w.owner, `select count(*) from jobs where kind = 'send_email' and payload->>'to' = 'alerts@example.com' and payload->>'subject' = $1`, subject)
	}

	ch := w.must(w.do(w.staffTok, "POST", "/v1/staff/actions/challenge", `{"kind":"suspend"}`), 200)
	suspendBody := actBody(ch["challenge_id"].(string), w.dev.assert(t, ch["options"]), "suspend", takedownReason)
	res := w.do(w.staffTok, "POST", "/v1/staff/actions", suspendBody)
	b := w.json(res)
	if res.StatusCode != 200 || b["kind"] != "suspend" || b["organizer_slug"] != "acme" || b["id"] == nil {
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
	// Spec test 28 end to end: give the seeded paid order a ticket to refund.
	if _, err := w.owner.Exec(context.Background(), `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents,
			tax_cents, fee_cents, charge_cents, secret)
		select organizer_id, event_id, id, $2, 2500, 0, 0, 100, 2500, decode(md5(random()::text), 'hex') from orders where id = $1`,
		w.fixture["order"], w.fixture["ticketType"]); err != nil {
		t.Fatal(err)
	}
	if b := w.actOn(w.staffTok, "refund_event", uuid.NewString()); b["status"] != 404 {
		t.Fatalf("refund of another organizer's event: %v", b)
	}
	b = w.actOn(w.staffTok, "refund_event", w.fixture["event"])
	want := map[string]any{"orders": 1.0, "amount_cents": 2600.0, "fee_cents": 100.0, "pending_orders": 0.0, "failed_orders": []any{}}
	if b["status"] != 202 || b["kind"] != "refund_event" || b["event"] != w.fixture["event"] || b["result"] != nil {
		t.Fatalf("refund_event: %v, want 202 with no result yet", b)
	}
	if r := w.refunded(b["id"].(string)); !jsonEqual(r, want) {
		t.Fatalf("refund_event result: %v", r)
	}
	if alerts("Staff refunded an event of acme") != 1 || alerts("Refund finished on acme") != 1 {
		t.Fatal("no refund alerts")
	}
	if n := countRows(t, w.owner, `select count(*) from refunds where reason = 'platform' and fee_refunded and amount_cents = 2600`); n != 1 {
		t.Fatalf("platform refunds = %d", n)
	}
	if n := countRows(t, w.owner, `select count(*) from staff_actions where kind = 'refund_event' and finished_at is not null`); n != 1 {
		t.Fatal("the refund action isn't finished")
	}
	if n := countRows(t, w.owner, `select count(*) from events where status = 'draft'`); n != 1 {
		t.Fatal("the event's status changed")
	}
	// Pressed again, it's a new action with a new touch: it finds nothing
	// left to refund. (already_done is for the same action twice, which
	// RefundEventForStaff's tests cover.)
	b = w.actOn(w.staffTok, "refund_event", w.fixture["event"])
	if b["status"] != 202 {
		t.Fatalf("refund_event again: %v", b)
	}
	if want["orders"], want["amount_cents"], want["fee_cents"] = 0.0, 0.0, 0.0; !jsonEqual(w.refunded(b["id"].(string)), want) {
		t.Fatalf("refund_event again: %v", b)
	}
	if n := countRows(t, w.owner, `select count(*) from refunds where reason = 'platform'`); n != 1 {
		t.Fatalf("platform refunds after a second press = %d", n)
	}

	if b := w.act(w.staffTok, "lift"); b["status"] != 200 || b["kind"] != "lift" {
		t.Fatalf("lift: %v", b)
	}
	if alerts("Staff lifted the suspension of acme") != 1 {
		t.Fatal("no lift alert")
	}
	if n := countRows(t, w.owner, `select count(*) from staff_actions where kind = 'lift' and reason = $1`, takedownReason); n != 1 {
		t.Fatalf("lift rows = %d", n)
	}
	if err := json.NewDecoder(w.do(w.ownerA, "GET", "/v1/organizers/acme/audit", "").Body).Decode(&audit); err != nil {
		t.Fatal(err)
	}
	lifted := 0
	for _, r := range audit {
		if r["action"] == "staff.lifted" && r["actor_type"] == "staff" && r["actor_email"] == nil {
			lifted++
		}
	}
	if lifted != 1 {
		t.Fatalf("staff.lifted rows the owner reads: %d", lifted)
	}
	// The suspend touch was spent: replaying it suspends nothing.
	if res := w.do(w.staffTok, "POST", "/v1/staff/actions", suspendBody); res.StatusCode != 403 || w.errorCode(res) != "passkey_required" {
		t.Fatalf("replayed suspend: %d", res.StatusCode)
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
		if s["id"] == w.session && (len(acts) != 4 || acts[0].(map[string]any)["kind"] != "suspend" || acts[1].(map[string]any)["finished_at"] == nil || asMap(acts[1].(map[string]any)["result"])["orders"] != 1.0 ||
			acts[3].(map[string]any)["kind"] != "lift") {
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

// With no alert address, neither action route works, and a touch already
// made isn't spent.
func TestStaffActionsNeedAnAlertAddress(t *testing.T) {
	ctx := context.Background()
	h := newStaffHarness(t, "")
	h.createOrganizer(h.signupWithMFA("owner@example.com"), "acme")
	dev, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	// Staff mode can't start without alerts either, so it's seeded.
	login := loginSession(t, h.owner, tok)
	var uid, org uuid.UUID
	if err := h.owner.QueryRow(ctx, `insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, expires_at)
		select u.id, $1, o.id, o.slug, o.name, 'Checking a takedown report', now() + interval '30 minutes'
		  from users u, organizers o where u.email = 'staff@example.com' and o.slug = 'acme' returning user_id, organizer_id`, login).Scan(&uid, &org); err != nil {
		t.Fatal(err)
	}
	if res := h.do(tok, "POST", "/v1/staff/actions/challenge", `{"kind":"suspend"}`); res.StatusCode != 503 || h.errorCode(res) != "alerts_off" {
		t.Fatalf("begin: %d %q", res.StatusCode, h.errorCode(res))
	}
	id, opts, err := h.auth.BeginStaffAssertion(ctx, uid, login, "staff_action", staff.Purpose("suspend", org, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res := h.do(tok, "POST", "/v1/staff/actions", actBody(id.String(), dev.assert(t, opts), "suspend", takedownReason)); res.StatusCode != 503 || h.errorCode(res) != "alerts_off" {
		t.Fatalf("take: %d %q", res.StatusCode, h.errorCode(res))
	}
	if n := countRows(t, h.owner, `select count(*) from webauthn_challenges where id = $1`, id); n != 1 {
		t.Fatal("the challenge was spent")
	}
	if n := countRows(t, h.owner, `select count(*) from staff_actions`); n != 0 {
		t.Fatalf("staff_actions = %d", n)
	}
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// Final review Minor 2: a staff action waits at most 5 seconds for the
// organizer row, then answers 503 try_again rather than queue every member
// write behind it.
func TestStaffActionGivesUpOnABusyOrganizer(t *testing.T) {
	ctx := context.Background()
	w := newStaffWorld(t)
	tx, err := w.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select 1 from organizers where slug = 'acme' for key share`); err != nil {
		t.Fatal(err)
	}
	ch := w.must(w.do(w.staffTok, "POST", "/v1/staff/actions/challenge", `{"kind":"suspend"}`), 200)
	start := time.Now()
	res := w.do(w.staffTok, "POST", "/v1/staff/actions", actBody(ch["challenge_id"].(string), w.dev.assert(t, ch["options"]), "suspend", takedownReason))
	if got := w.errorCode(res); res.StatusCode != 503 || got != "try_again" || res.Header.Get("Retry-After") == "" || time.Since(start) > 15*time.Second {
		t.Fatalf("suspend behind a busy organizer: %d %q Retry-After %q after %v, want 503 try_again", res.StatusCode, got, res.Header.Get("Retry-After"), time.Since(start))
	}
	if n := countRows(t, w.owner, `select count(*) from staff_actions`); n != 0 {
		t.Fatalf("staff_actions = %d, want none", n)
	}
	_ = tx.Rollback(ctx)
	if b := w.act(w.staffTok, "suspend"); b["status"] != 200 {
		t.Fatalf("suspend once the organizer is free: %v", b)
	}
}
