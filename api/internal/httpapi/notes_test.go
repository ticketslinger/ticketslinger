package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (s shop) notesOf(who, order string) []map[string]any {
	s.t.Helper()
	res := s.do(who, "GET", s.orderPath(order)+"/notes", "")
	b, _ := io.ReadAll(res.Body)
	var out []map[string]any
	if res.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		s.t.Fatalf("list notes: %d %s", res.StatusCode, b)
	}
	return out
}

func (s shop) addNote(who, order, body string) *http.Response {
	s.t.Helper()
	b, _ := json.Marshal(map[string]string{"body": body})
	return s.do(who, "POST", s.orderPath(order)+"/notes", string(b))
}

// Staff mode and suspension are refused by StaffModeWalk and SuspensionWalk,
// which pick the three operations up from the spec.
func TestNoteRoles(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "notes@example.com", "evt_note_roles")
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	admin2 := s.member("admin2@example.com", "admin")
	door := s.member("door@example.com", "checkin")
	noMFA := s.member("nomfa@example.com", "admin-no-mfa")

	for _, c := range []struct {
		who, code string
	}{{writeKey, "session_required"}, {door, "forbidden_role"}, {noMFA, "mfa_required"}} {
		if res := s.addNote(c.who, order, "nope"); res.StatusCode != 403 || s.errorCode(res) != c.code {
			t.Errorf("add as %s: %d %q", c.code, res.StatusCode, s.errorCode(res))
		}
		if res := s.do(c.who, "GET", s.orderPath(order)+"/notes", ""); res.StatusCode != 403 || (c.code != "mfa_required" && s.errorCode(res) != c.code) {
			t.Errorf("list as %s: %d %q", c.code, res.StatusCode, s.errorCode(res))
		}
	}
	if n := s.count(`select count(*) from order_notes where order_id = $1`, order); n != 0 {
		t.Fatalf("refused callers wrote %d notes", n)
	}

	own := s.must(s.addNote(s.owner, order, "Called about parking"), 201)
	adm := s.must(s.addNote(admin, order, "Dana rang back"), 201)
	if own["can_delete"] != true || adm["author_name"] == "" {
		t.Fatalf("created %v %v", own, adm)
	}
	// can_delete: yours, or any as owner.
	for who, want := range map[string][]bool{s.owner: {true, true}, admin: {true, false}, admin2: {false, false}} {
		got := s.notesOf(who, order)
		if len(got) != 2 || got[0]["id"] != adm["id"] || got[0]["can_delete"] != want[0] || got[1]["can_delete"] != want[1] {
			t.Errorf("notes %v, want can_delete %v", got, want)
		}
	}
	notePath := func(n map[string]any) string { return s.orderPath(order) + "/notes/" + n["id"].(string) }
	if res := s.do(admin, "DELETE", notePath(own), ""); res.StatusCode != 403 || s.errorCode(res) != "not_your_note" {
		t.Fatalf("admin deleting owner's note: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(writeKey, "DELETE", notePath(adm), ""); res.StatusCode != 403 || s.errorCode(res) != "session_required" {
		t.Fatalf("key delete: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(door, "DELETE", notePath(adm), ""); res.StatusCode != 403 || s.errorCode(res) != "forbidden_role" {
		t.Fatalf("door delete: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(s.owner, "DELETE", notePath(adm), ""); res.StatusCode != 204 {
		t.Fatalf("owner delete: %d", res.StatusCode)
	}
	if res := s.do(s.owner, "DELETE", notePath(adm), ""); res.StatusCode != 404 {
		t.Fatalf("second delete: %d, want 404", res.StatusCode)
	}
	if res := s.do(admin, "DELETE", notePath(own), ""); res.StatusCode != 403 {
		t.Fatalf("still not theirs: %d", res.StatusCode)
	}
	if got := s.notesOf(s.owner, order); len(got) != 1 || got[0]["id"] != own["id"] {
		t.Fatalf("after delete %v", got)
	}
	for _, body := range []string{"", strings.Repeat("a", 2001)} {
		if res := s.addNote(s.owner, order, body); res.StatusCode != 422 || s.errorCode(res) != "bad_note" {
			t.Errorf("%d chars: %d %q", len(body), res.StatusCode, s.errorCode(res))
		}
	}
}

// A manager of one organizer can't see, add or delete a note on another's order.
func TestNotesStayInTheirOrganizer(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "notes@example.com", "evt_note_tenant")
	note := s.must(s.addNote(s.owner, order, "Ours"), 201)["id"].(string)
	other := s.signupWithMFA("beta-owner@example.com")
	s.createOrganizer(other, "beta")
	// Through beta's own slug with acme's ids, and through acme's slug.
	for _, org := range []string{"beta", "acme"} {
		base := strings.Replace(s.orderPath(order), "acme", org, 1) + "/notes"
		for _, c := range []struct{ method, path, body string }{
			{"GET", base, ""}, {"POST", base, `{"body":"planted"}`}, {"DELETE", base + "/" + note, ""},
		} {
			if res := s.do(other, c.method, c.path, c.body); res.StatusCode != 404 {
				t.Errorf("%s %s: %d, want 404", c.method, c.path, res.StatusCode)
			}
		}
	}
	if got := s.notesOf(s.owner, order); len(got) != 1 || got[0]["body"] != "Ours" {
		t.Fatalf("notes after the other tenant's tries: %v", got)
	}
}

// Review Focus 5: a note is text. It comes back as typed, and nothing of it
// reaches the order JSON the dashboard builds from or an export.
func TestNoteIsPlainText(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "notes@example.com", "evt_note_plain")
	body := "<img src=x onerror=alert(1)>\n**bold**"
	s.must(s.addNote(s.owner, order, body), 201)
	if got := s.notesOf(s.owner, order); len(got) != 1 || got[0]["body"] != body {
		t.Fatalf("notes %v, want the body back as typed", got)
	}
	res := s.do(s.owner, "GET", s.orderPath(order), "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || strings.Contains(string(b), "onerror") || strings.Contains(string(b), "**bold**") {
		t.Fatalf("order detail %d carries the note: %s", res.StatusCode, b)
	}
	for _, kind := range []string{"orders.csv", "attendees.csv", "checkins.csv"} {
		res := s.do(s.owner, "GET", "/v1/organizers/acme/events/"+s.f["event"]+"/exports/"+kind, "")
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 || strings.Contains(string(b), "onerror") || strings.Contains(string(b), "**bold**") {
			t.Errorf("%s %d carries the note", kind, res.StatusCode)
		}
	}
}
