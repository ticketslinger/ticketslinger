package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

const eventBody = `{"slug":"spring-gala","name":"Spring Gala","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`

func TestEventRoundTrip(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")

	ev := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)
	id := ev["id"].(string)
	if ev["status"] != "draft" || ev["fee_mode"] != "pass" || ev["currency"] != "USD" {
		t.Fatalf("created: %v", ev)
	}
	h.must(h.do(owner, "GET", "/v1/organizers/acme/events/"+id, ""), 200)
	if res := h.do(owner, "POST", "/v1/organizers/acme/events", eventBody); res.StatusCode != 409 || h.errorCode(res) != "slug_taken" {
		t.Fatalf("duplicate: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "POST", "/v1/organizers/acme/events", `{"slug":"x","name":"X","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`); res.StatusCode != 422 || h.errorCode(res) != "invalid_slug" {
		t.Fatalf("bad slug: %d %q", res.StatusCode, h.errorCode(res))
	}
	up := h.must(h.do(owner, "PATCH", "/v1/organizers/acme/events/"+id, `{"name":"Autumn Gala","tax_rate_bp":825,"fee_mode":"absorb"}`), 200)
	if up["name"] != "Autumn Gala" || up["tax_rate_bp"] != float64(825) || up["fee_mode"] != "absorb" || up["slug"] != "spring-gala" {
		t.Fatalf("patched: %v", up)
	}
	for status, code := range map[string]string{"live": "no_ticket_types", "ended": "status_not_allowed"} {
		if res := h.do(owner, "PATCH", "/v1/organizers/acme/events/"+id, `{"status":"`+status+`"}`); res.StatusCode != 422 || h.errorCode(res) != code {
			t.Errorf("status %s: %d %q, want 422 %s", status, res.StatusCode, h.errorCode(res), code)
		}
	}
	res := h.do(owner, "GET", "/v1/organizers/acme/events", "")
	var list []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&list)
	if res.StatusCode != 200 || len(list) != 1 || list[0]["id"] != id {
		t.Fatalf("list: %d %v", res.StatusCode, list)
	}
	h.must(h.do(owner, "DELETE", "/v1/organizers/acme/events/"+id, ""), 204)
	h.must(h.do(owner, "GET", "/v1/organizers/acme/events/"+id, ""), 404)
	h.must(h.do(owner, "DELETE", "/v1/organizers/acme/events/"+id, ""), 404)
}

func TestCheckinMemberReadsEventsButCannotWrite(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	id := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	staff := h.signupWithMFA("staff@example.com")
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(),
		`INSERT INTO memberships (organizer_id, user_id, role)
		 SELECT o.id, u.id, 'checkin' FROM organizers o, users u WHERE o.slug = 'acme' AND u.email = 'staff@example.com'`); err != nil {
		t.Fatal(err)
	}
	h.must(h.do(staff, "GET", "/v1/organizers/acme/events", ""), 200)
	h.must(h.do(staff, "GET", "/v1/organizers/acme/events/"+id, ""), 200)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/organizers/acme/events", eventBody},
		{"PATCH", "/v1/organizers/acme/events/" + id, `{"name":"Nope"}`},
		{"DELETE", "/v1/organizers/acme/events/" + id, ""},
	} {
		if res := h.do(staff, c.method, c.path, c.body); res.StatusCode != 403 || h.errorCode(res) != "forbidden_role" {
			t.Errorf("%s %s as checkin: %d %q, want 403 forbidden_role", c.method, c.path, res.StatusCode, h.errorCode(res))
		}
	}
}

func TestEventsAndAPIKeyScopes(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	id := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	_, readKey := h.createKey(owner, "acme", "read")
	writeID, writeKey := h.createKey(owner, "acme", "write")

	h.must(h.do(readKey, "GET", "/v1/organizers/acme/events", ""), 200)
	h.must(h.do(readKey, "GET", "/v1/organizers/acme/events/"+id, ""), 200)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/organizers/acme/events", eventBody},
		{"PATCH", "/v1/organizers/acme/events/" + id, `{"name":"Nope"}`},
		{"DELETE", "/v1/organizers/acme/events/" + id, ""},
	} {
		if res := h.do(readKey, c.method, c.path, c.body); res.StatusCode != 403 || h.errorCode(res) != "scope_read_only" {
			t.Errorf("%s as read key: %d %q, want 403 scope_read_only", c.method, res.StatusCode, h.errorCode(res))
		}
	}

	// A change made with a write key is the key's, not a person's, in the log.
	h.must(h.do(writeKey, "PATCH", "/v1/organizers/acme/events/"+id, `{"name":"Renamed"}`), 200)
	res := h.do(owner, "GET", "/v1/organizers/acme/audit", "")
	var rows []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&rows)
	if len(rows) == 0 || rows[0]["action"] != "event.updated" || rows[0]["actor_api_key_id"] != writeID || rows[0]["actor_user_id"] != nil {
		t.Fatalf("audit: %v", rows)
	}
	h.must(h.do(writeKey, "DELETE", "/v1/organizers/acme/events/"+id, ""), 204)
}

func TestEventOfAnotherOrganizerIs404(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.createOrganizer(owner, "beta")
	id := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"DELETE", ""}} {
		if res := h.do(owner, c.method, "/v1/organizers/beta/events/"+id, c.body); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s via the wrong organizer: %d, want 404", c.method, res.StatusCode)
		}
	}
}
