package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// startStaff puts tok's login session in staff mode on slug and returns the
// staff session's id.
func startStaff(h staffHarness, dev *passkeyDevice, tok, slug string) string {
	h.t.Helper()
	ch := h.must(h.do(tok, "POST", "/v1/staff/sessions/challenge", ""), 200)
	body := strings.Replace(startBody(ch["challenge_id"].(string), dev.assert(h.t, ch["options"]), goodReason), `"organizer":"acme"`, `"organizer":"`+slug+`"`, 1)
	return h.must(h.do(tok, "POST", "/v1/staff/sessions", body), 201)["id"].(string)
}

type staffWorld struct {
	staffHarness
	ownerA, ownerB, stranger string
	staffTok, staffElsewhere string // in staff mode on acme; on bravo
	staffIdle                string // the same staff member, not in staff mode
	session                  string // staffTok's staff session
	fixture                  map[string]string
}

// newStaffWorld: acme with one of everything, bravo with nothing, and one
// staff member (named Sam Support) signed in three times.
func newStaffWorld(t *testing.T) staffWorld {
	h := newStaffHarness(t, "alerts@example.com")
	w := staffWorld{staffHarness: h}
	w.ownerA = h.signupWithMFA("owner@example.com")
	h.createOrganizer(w.ownerA, "acme")
	keyID, _ := h.createKey(w.ownerA, "acme", "write")
	w.fixture = seedCatalogue(h.harness, w.ownerA, "acme", keyID)
	w.ownerB = h.signupWithMFA("bravo@example.com")
	h.createOrganizer(w.ownerB, "bravo")
	w.stranger = h.signupWithMFA("stranger@example.com")

	dev, tok := h.signupWithPasskey("staff@example.com")
	grantStaff(t, h.owner, "staff@example.com")
	if _, err := h.owner.Exec(context.Background(), `update users set name = 'Sam Support' where email = 'staff@example.com'`); err != nil {
		t.Fatal(err)
	}
	w.staffTok, w.staffElsewhere, w.staffIdle = tok, h.passkeyLogin(dev), h.passkeyLogin(dev)
	w.session = startStaff(h, dev, w.staffTok, "acme")
	startStaff(h, dev, w.staffElsewhere, "bravo")
	return w
}

// notFoundBody is what an organizer route answers a non-member.
var notFoundBody = map[string]any{"error": map[string]any{"code": "not_found", "message": "That doesn't exist, or you don't have access to it."}}

func accessRows(t *testing.T, owner *pgxpool.Pool, session string) int {
	t.Helper()
	return countRows(t, owner, `select count(*) from staff_access_log where staff_session_id = $1`, session)
}

// Test 10: every organizer operation, as four callers who aren't members.
// Only staff in staff mode on this organizer get in, to read; every request
// they make is logged first, refused ones included.
func TestStaffModeWalk(t *testing.T) {
	w := newStaffWorld(t)
	called := 0
	eachOp(t, w.fixture, func(id, method, path, url string) {
		if !strings.HasPrefix(path, "/organizers/{organizer}") {
			return
		}
		for name, tok := range map[string]string{"non-staff": w.stranger, "staff not in staff mode": w.staffIdle, "staff on another organizer": w.staffElsewhere} {
			res := w.do(tok, method, "/v1"+url, `{}`)
			if b := w.json(res); res.StatusCode != 404 || !jsonEqual(b, notFoundBody) {
				t.Errorf("%s: %s %s got %d %v, want 404 not_found", name, method, url, res.StatusCode, b)
			}
		}
		wantStatus, wantCode := http.StatusForbidden, "staff_read_only"
		switch {
		case id == "exportEventCsv":
			wantCode = "staff_not_allowed"
		case method == "GET":
			wantStatus, wantCode = w.do(w.ownerA, method, "/v1"+url, "").StatusCode, ""
			if wantStatus >= 300 {
				t.Errorf("owner: %s %s (%s) got %d; seed a fixture so the read succeeds", method, url, id, wantStatus)
			}
		}
		res := w.do(w.staffTok, method, "/v1"+url, `{}`)
		if got := w.errorCode(res); res.StatusCode != wantStatus || (wantCode != "" && got != wantCode) {
			t.Errorf("staff mode: %s %s (%s): got %d %q, want %d %q", method, url, id, res.StatusCode, got, wantStatus, wantCode)
		}
		called++
	})
	// The slug matches in any case, as it does for members.
	w.must(w.do(w.staffTok, "GET", "/v1/organizers/ACME/events", ""), 200)
	called++
	if called < 40 {
		t.Fatalf("walked %d organizer operations; the walk is broken", called)
	}
	if n := accessRows(t, w.owner, w.session); n != called {
		t.Errorf("staff_access_log has %d rows for %d staff-mode requests", n, called)
	}
	if n := countRows(t, w.owner, `select count(*) from staff_access_log where staff_session_id <> $1`, w.session); n != 0 {
		t.Errorf("%d requests logged against the other staff session, which never got in", n)
	}
}

// Test 13: the log row is written, and committed, before the handler runs;
// when it can't be, the handler doesn't run.
func TestStaffRequestIsLoggedFirst(t *testing.T) {
	w := newStaffWorld(t)
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	report := "/v1/organizers/acme/events/" + w.fixture["event"] + "/report"
	b := w.must(w.do(w.staffTok, "GET", report+"?from=email", ""), 200)
	if _, ok := b["gross_cents"]; !ok {
		t.Fatalf("report: %v", b)
	}
	route := "getEventReport organizer=acme event=" + w.fixture["event"]
	if n := countRows(t, w.owner, `select count(*) from staff_access_log where staff_session_id = $1 and method = 'GET' and route = $2`, w.session, route); n != 1 {
		t.Fatalf("log rows for %q: %d, want 1", route, n)
	}
	if n := accessRows(t, w.owner, w.session); n != 1 {
		t.Fatalf("log rows: %d, want 1", n)
	}
	if !strings.Contains(logs.String(), "msg=staff_access") || !strings.Contains(logs.String(), "staff_session="+w.session) {
		t.Fatalf("no staff_access line: %s", logs.String())
	}

	// A refused write gets its journald line too, saying so.
	w.must(w.do(w.staffTok, "POST", "/v1/organizers/acme/events", `{}`), 403)
	if !strings.Contains(logs.String(), "route=createEvent organizer=acme") && !strings.Contains(logs.String(), `route="createEvent organizer=acme"`) {
		t.Fatalf("no staff_access line for the refused write: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "outcome=staff_read_only") || !strings.Contains(logs.String(), "outcome=read") {
		t.Fatalf("staff_access lines don't say what happened: %s", logs.String())
	}
	if n := accessRows(t, w.owner, w.session); n != 2 {
		t.Fatalf("log rows: %d, want 2", n)
	}

	if _, err := w.owner.Exec(context.Background(), `revoke execute on function log_staff_access(uuid, text, text) from ts_app`); err != nil {
		t.Fatal(err)
	}
	before := strings.Count(logs.String(), "msg=staff_access ")
	res := w.do(w.staffTok, "GET", report, "")
	b = w.json(res)
	// The error envelope and nothing else: no report fields came with it.
	if e, _ := b["error"].(map[string]any); res.StatusCode != http.StatusServiceUnavailable || len(b) != 1 || e["code"] != "staff_log_unavailable" {
		t.Fatalf("with the log refused: got %d %v, want 503 staff_log_unavailable and only the error envelope", res.StatusCode, b)
	}
	if n := accessRows(t, w.owner, w.session); n != 2 {
		t.Fatalf("log rows after the refusal: %d, want 2", n)
	}
	if after := strings.Count(logs.String(), "msg=staff_access "); after != before {
		t.Fatalf("staff_access logged after the log write failed: %s", logs.String())
	}
}

// Test 14: owners and admins see the staff sessions on their own organizer,
// with names but never emails, and nobody else's.
func TestOrganizerSeesOnlyItsStaffAccess(t *testing.T) {
	w := newStaffWorld(t)
	w.must(w.do(w.staffTok, "GET", "/v1/organizers/acme/events", ""), 200)

	list := func(tok, slug string) (int, []map[string]any, string) {
		t.Helper()
		res := w.do(tok, "GET", "/v1/organizers/"+slug+"/staff-access", "")
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(res.Body)
		var out []map[string]any
		_ = json.Unmarshal(raw.Bytes(), &out)
		return res.StatusCode, out, raw.String()
	}
	status, rows, raw := list(w.ownerA, "acme")
	if status != 200 || len(rows) != 1 {
		t.Fatalf("acme's owner: %d %s", status, raw)
	}
	r := rows[0]
	reqs, _ := r["requests"].([]any)
	if r["id"] != w.session || r["staff_name"] != "Sam Support" || r["reason"] != goodReason || len(reqs) != 1 ||
		reqs[0].(map[string]any)["route"] != "listEvents organizer=acme" {
		t.Errorf("acme's staff access: %s", raw)
	}
	if strings.Contains(raw, "staff@example.com") || strings.Contains(raw, "staff_email") {
		t.Errorf("an organizer sees a staff email: %s", raw)
	}

	if status, rows, raw := list(w.ownerB, "bravo"); status != 200 || len(rows) != 1 || strings.Contains(raw, "listEvents") || strings.Contains(raw, w.session) {
		t.Errorf("bravo's owner: %d %s", status, raw)
	}
	// Staff see every staff session elsewhere; here, only this organizer's.
	if status, rows, raw := list(w.staffTok, "acme"); status != 200 || len(rows) != 1 || rows[0]["organizer_slug"] != "acme" {
		t.Errorf("staff mode on acme: %d %s", status, raw)
	}
	if status, _, _ := list(w.ownerB, "acme"); status != 404 {
		t.Errorf("bravo's owner on acme: %d, want 404", status)
	}

	checkin := w.signupWithMFA("door@example.com")
	if _, err := w.owner.Exec(context.Background(), `insert into memberships (organizer_id, user_id, role)
		select o.id, u.id, 'checkin' from organizers o, users u where o.slug = 'acme' and u.email = 'door@example.com'`); err != nil {
		t.Fatal(err)
	}
	res := w.do(checkin, "GET", "/v1/organizers/acme/staff-access", "")
	if res.StatusCode != 403 || w.errorCode(res) != "forbidden_role" {
		t.Errorf("a check-in member: %d %q, want 403 forbidden_role", res.StatusCode, w.errorCode(res))
	}
}

// The organizer's audit page names staff as staff, by name, without an email.
func TestAuditNamesStaff(t *testing.T) {
	w := newStaffWorld(t)
	var all []map[string]any
	res := w.do(w.ownerA, "GET", "/v1/organizers/acme/audit", "")
	if err := json.NewDecoder(res.Body).Decode(&all); err != nil || res.StatusCode != 200 {
		t.Fatalf("audit: %d %v", res.StatusCode, err)
	}
	found := false
	for _, r := range all {
		if r["action"] != "staff.session_started" {
			continue
		}
		found = true
		if r["actor_type"] != "staff" || r["actor_name"] != "Sam Support" || r["actor_email"] != nil {
			t.Errorf("staff row: %v", r)
		}
	}
	if !found {
		t.Fatalf("no staff.session_started row: %v", all)
	}
}
