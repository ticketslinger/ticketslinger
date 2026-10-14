package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

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
	if res := h.do(owner, "PATCH", "/v1/organizers/acme/events/"+id, `{"ends_at":"2027-03-01T18:00:00Z"}`); res.StatusCode != 422 || h.errorCode(res) != "bad_times" {
		t.Errorf("bad_times: %d %q", res.StatusCode, h.errorCode(res))
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

func TestTicketTypeRoundTrip(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	ev := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	base := "/v1/organizers/acme/events/" + ev + "/ticket-types"

	if res := h.do(owner, "POST", base, `{"name":"Free","price_cents":-5}`); res.StatusCode != 422 || h.errorCode(res) != "bad_price" {
		t.Fatalf("bad price: %d %q", res.StatusCode, h.errorCode(res))
	}
	tk := h.must(h.do(owner, "POST", base, `{"name":"General","price_cents":2500}`), 201)
	id := tk["id"].(string)
	if tk["price_cents"] != float64(2500) || tk["min_per_order"] != float64(1) || tk["max_per_order"] != float64(10) || tk["position"] != float64(1) {
		t.Fatalf("created: %v", tk)
	}
	h.must(h.do(owner, "POST", base, `{"name":"VIP","price_cents":9000}`), 201)
	up := h.must(h.do(owner, "PATCH", base+"/"+id, `{"price_cents":3000,"tax_rate_bp":825}`), 200)
	if up["price_cents"] != float64(3000) || up["tax_rate_bp"] != float64(825) || up["name"] != "General" {
		t.Fatalf("patched: %v", up)
	}
	if res := h.do(owner, "PATCH", base+"/"+id, `{"min_per_order":50}`); res.StatusCode != 422 || h.errorCode(res) != "bad_limits" {
		t.Fatalf("bad limits: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(owner, "GET", base+"/"+id, ""), 200)
	for _, body := range []string{`{"name":"Z","price_cents":1,"min_per_order":0}`, `{"name":"Z","price_cents":1,"max_per_order":0}`, `{"name":"Z","price_cents":1,"max_per_order":101}`} {
		if res := h.do(owner, "POST", base, body); res.StatusCode != 422 || h.errorCode(res) != "bad_limits" {
			t.Fatalf("%s: %d %q", body, res.StatusCode, h.errorCode(res))
		}
	}
	// null clears, absent keeps, and the response always carries the field.
	w := h.must(h.do(owner, "PATCH", base+"/"+id, `{"sales_start":"2027-01-01T00:00:00Z","sales_end":"2027-02-01T00:00:00Z","prices_include_tax":true}`), 200)
	if w["sales_start"] == nil || w["prices_include_tax"] != true {
		t.Fatalf("set: %v", w)
	}
	c := h.must(h.do(owner, "PATCH", base+"/"+id, `{"tax_rate_bp":null,"sales_start":null,"prices_include_tax":null}`), 200)
	for _, f := range []string{"tax_rate_bp", "sales_start", "prices_include_tax"} {
		if v, ok := c[f]; !ok || v != nil {
			t.Fatalf("%s should be present and null: %v", f, c)
		}
	}
	if end, _ := time.Parse(time.RFC3339, c["sales_end"].(string)); !end.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("absent sales_end changed: %v", c)
	}
	var list []map[string]any
	res := h.do(owner, "GET", base, "")
	_ = json.NewDecoder(res.Body).Decode(&list)
	if res.StatusCode != 200 || len(list) != 2 || list[0]["id"] != id {
		t.Fatalf("list: %d %v", res.StatusCode, list)
	}

	// Live, then the last ticket type is protected.
	h.must(h.do(owner, "PATCH", "/v1/organizers/acme/events/"+ev, `{"status":"live"}`), 200)
	h.must(h.do(owner, "DELETE", base+"/"+id, ""), 204)
	var other string
	for _, x := range list {
		if x["id"] != id {
			other = x["id"].(string)
		}
	}
	if res := h.do(owner, "DELETE", base+"/"+other, ""); res.StatusCode != 409 || h.errorCode(res) != "last_ticket_type_live" {
		t.Fatalf("delete last live: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(owner, "DELETE", base+"/"+id, ""), 404)
}

func TestTicketTypesUnderAnotherTenantsEvent(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.createOrganizer(owner, "other")
	ev := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	tk := h.must(h.do(owner, "POST", "/v1/organizers/acme/events/"+ev+"/ticket-types", `{"name":"General","price_cents":2500}`), 201)["id"].(string)
	// The caller owns both organizers, so only the event's own tenant can answer.
	base := "/v1/organizers/other/events/" + ev + "/ticket-types"
	h.must(h.do(owner, "POST", base, `{"name":"Sneak","price_cents":1}`), 404)
	h.must(h.do(owner, "PATCH", base+"/"+tk, `{"name":"Sneak"}`), 404)
	h.must(h.do(owner, "GET", base, ""), 404)
	h.must(h.do(owner, "DELETE", base+"/"+tk, ""), 404)
}
