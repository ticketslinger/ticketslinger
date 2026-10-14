package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// eventBody is next year, so it hasn't ended when a test puts it on sale.
var eventBody = strings.ReplaceAll(`{"slug":"spring-gala","name":"Spring Gala","timezone":"Europe/London","starts_at":"YYYY-03-01T19:00:00Z","ends_at":"YYYY-03-01T22:00:00Z"}`,
	"YYYY", strconv.Itoa(time.Now().Year()+1))

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
	for _, body := range []string{`{"name":"Z","price_cents":100,"min_per_order":0}`, `{"name":"Z","price_cents":100,"max_per_order":0}`, `{"name":"Z","price_cents":100,"max_per_order":101}`} {
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

	// Live (every ticket type needs a quota first), then the last ticket type is protected.
	h.must(h.do(owner, "POST", "/v1/organizers/acme/events/"+ev+"/quotas",
		`{"name":"Hall","size":100,"ticket_type_ids":["`+list[0]["id"].(string)+`","`+list[1]["id"].(string)+`"]}`), 201)
	h.chargesEnabled("acme")
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
	h.must(h.do(owner, "POST", base, `{"name":"Sneak","price_cents":100}`), 404)
	h.must(h.do(owner, "PATCH", base+"/"+tk, `{"name":"Sneak"}`), 404)
	h.must(h.do(owner, "GET", base, ""), 404)
	h.must(h.do(owner, "DELETE", base+"/"+tk, ""), 404)
}

func TestQuotaOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.createOrganizer(owner, "other")
	ev := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	tk := h.must(h.do(owner, "POST", "/v1/organizers/acme/events/"+ev+"/ticket-types", `{"name":"General","price_cents":2500}`), 201)["id"].(string)
	oev := h.must(h.do(owner, "POST", "/v1/organizers/other/events", eventBody), 201)["id"].(string)
	otk := h.must(h.do(owner, "POST", "/v1/organizers/other/events/"+oev+"/ticket-types", `{"name":"Theirs","price_cents":100}`), 201)["id"].(string)
	oq := h.must(h.do(owner, "POST", "/v1/organizers/other/events/"+oev+"/quotas", `{"name":"Theirs","size":5}`), 201)["id"].(string)
	base := "/v1/organizers/acme/events/" + ev + "/quotas"

	if res := h.do(owner, "POST", base, `{"name":"Hall","size":10,"ticket_type_ids":["`+otk+`"]}`); res.StatusCode != 422 || h.errorCode(res) != "unknown_ticket_type" {
		t.Fatalf("foreign ticket type: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "POST", base, `{"name":"Hall"}`); res.StatusCode != 422 || h.errorCode(res) != "missing_field" {
		t.Fatalf("missing size: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "POST", "/v1/organizers/acme/events/"+ev+"/ticket-types", `{"name":"Free?"}`); res.StatusCode != 422 || h.errorCode(res) != "missing_field" {
		t.Fatalf("missing price: %d %q", res.StatusCode, h.errorCode(res))
	}
	comp := h.must(h.do(owner, "POST", "/v1/organizers/acme/events/"+ev+"/ticket-types", `{"name":"Comp","price_cents":0}`), 201)["id"].(string)
	h.must(h.do(owner, "POST", base, `{"name":"Zero","size":0}`), 201)
	if res := h.do(owner, "POST", base, `{"name":"Hall","size":-1}`); res.StatusCode != 422 || h.errorCode(res) != "bad_quota_size" {
		t.Fatalf("size: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "PATCH", "/v1/organizers/acme/events/"+ev, `{"status":"live"}`); res.StatusCode != 422 || h.errorCode(res) != "unquotaed_ticket_type" {
		t.Fatalf("live without quota: %d %q", res.StatusCode, h.errorCode(res))
	}
	q := h.must(h.do(owner, "POST", base, `{"name":"Hall","size":10,"ticket_type_ids":["`+tk+`"]}`), 201)
	id := q["id"].(string)
	if ids, _ := q["ticket_type_ids"].([]any); len(ids) != 1 || ids[0] != tk {
		t.Fatalf("created: %v", q)
	}
	if c := h.must(h.do(owner, "PATCH", base+"/"+id, `{"size":20}`), 200); c["size"] != float64(20) || len(c["ticket_type_ids"].([]any)) != 1 {
		t.Fatalf("patch size: %v", c)
	}
	// "Comp" and "Zero" above: cover Comp, then go live.
	h.must(h.do(owner, "PATCH", base+"/"+id, `{"ticket_type_ids":["`+tk+`","`+comp+`"]}`), 200)
	h.chargesEnabled("acme")
	h.must(h.do(owner, "PATCH", "/v1/organizers/acme/events/"+ev, `{"status":"live"}`), 200)
	tts := "/v1/organizers/acme/events/" + ev + "/ticket-types"
	if res := h.do(owner, "POST", tts, `{"name":"Late","price_cents":100}`); res.StatusCode != 409 || h.errorCode(res) != "live_needs_capacity" {
		t.Fatalf("live create without quotas: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "POST", tts, `{"name":"Late","price_cents":100,"quota_ids":["`+oq+`"]}`); res.StatusCode != 422 || h.errorCode(res) != "unknown_quota" {
		t.Fatalf("live create with foreign quota: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(owner, "POST", tts, `{"name":"Late","price_cents":100,"quota_ids":["`+id+`"]}`), 201)
	if res := h.do(owner, "PATCH", base+"/"+id, `{"ticket_type_ids":[]}`); res.StatusCode != 409 || h.errorCode(res) != "live_needs_capacity" {
		t.Fatalf("unlink on live: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "DELETE", base+"/"+id, ""); res.StatusCode != 409 || h.errorCode(res) != "live_needs_capacity" {
		t.Fatalf("delete on live: %d %q", res.StatusCode, h.errorCode(res))
	}
	// Only the event's own tenant answers.
	other := "/v1/organizers/other/events/" + ev + "/quotas"
	h.must(h.do(owner, "GET", other, ""), 404)
	h.must(h.do(owner, "GET", other+"/"+id, ""), 404)
	h.must(h.do(owner, "DELETE", other+"/"+id, ""), 404)
	h.must(h.do(owner, "PATCH", "/v1/organizers/acme/events/"+ev, `{"status":"draft"}`), 200)
	h.must(h.do(owner, "DELETE", base+"/"+id, ""), 204)
}

func TestPromoCodeOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	base := "/v1/organizers/acme/events/" + h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	secret := h.must(h.do(owner, "POST", base+"/ticket-types", `{"name":"Secret","price_cents":0,"hidden":true}`), 201)["id"].(string)
	visible := h.must(h.do(owner, "POST", base+"/ticket-types", `{"name":"General","price_cents":2500}`), 201)["id"].(string)

	for body, field := range map[string]string{`{"kind":"percent","value":10}`: "code", `{"code":"ABC","value":10}`: "kind", `{"code":"ABC","kind":"percent"}`: "value"} {
		if res := h.do(owner, "POST", base+"/promo-codes", body); res.StatusCode != 422 || h.errorCode(res) != "missing_field" {
			t.Errorf("missing %s: %d %q", field, res.StatusCode, h.errorCode(res))
		}
	}
	if res := h.do(owner, "POST", base+"/promo-codes", `{"code":"VIS","kind":"percent","value":1000,"unlocks":["`+visible+`"]}`); res.StatusCode != 422 || h.errorCode(res) != "unlock_not_hidden" {
		t.Fatalf("visible unlock: %d %q", res.StatusCode, h.errorCode(res))
	}
	p := h.must(h.do(owner, "POST", base+"/promo-codes", `{"code":" early ","kind":"percent","value":1000,"max_uses":3,"unlocks":["`+secret+`"],"used_count":9}`), 201)
	id := p["id"].(string)
	if p["code"] != "EARLY" || p["used_count"] != float64(0) || p["max_uses"] != float64(3) || p["valid_from"] != nil || p["applies_to_all"] != true {
		t.Fatalf("created: %v", p)
	}
	if res := h.do(owner, "POST", base+"/promo-codes", `{"code":"early","kind":"amount","value":500}`); res.StatusCode != 409 || h.errorCode(res) != "code_taken" {
		t.Fatalf("duplicate: %d %q", res.StatusCode, h.errorCode(res))
	}
	up := h.must(h.do(owner, "PATCH", base+"/promo-codes/"+id, `{"max_uses":null,"used_count":5}`), 200)
	if up["max_uses"] != nil || up["used_count"] != float64(0) || len(up["unlocks"].([]any)) != 1 {
		t.Fatalf("patched: %v", up)
	}
	if res := h.do(owner, "PATCH", base+"/ticket-types/"+secret, `{"hidden":false}`); res.StatusCode != 409 || h.errorCode(res) != "unlocked_by_promo" {
		t.Fatalf("show unlocked: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.do(owner, "PATCH", base+"/promo-codes/"+id, `{"kind":"amount"}`); res.StatusCode != 422 || h.errorCode(res) != "value_required_with_kind" {
		t.Fatalf("kind alone: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(owner, "GET", base+"/promo-codes/"+id, ""), 200)
	h.must(h.do(owner, "DELETE", base+"/promo-codes/"+id, ""), 204)
	h.must(h.do(owner, "GET", base+"/promo-codes/"+id, ""), 404)
}

func TestQuestionOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	base := "/v1/organizers/acme/events/" + h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	for body, field := range map[string]string{`{"kind":"text","scope":"order"}`: "label", `{"label":"Q","scope":"order"}`: "kind", `{"label":"Q","kind":"text"}`: "scope"} {
		if res := h.do(owner, "POST", base+"/questions", body); res.StatusCode != 422 || h.errorCode(res) != "missing_field" {
			t.Errorf("missing %s: %d %q", field, res.StatusCode, h.errorCode(res))
		}
	}
	if res := h.do(owner, "POST", base+"/questions", `{"label":"Q","kind":"select","scope":"order","options":["a"]}`); res.StatusCode != 422 || h.errorCode(res) != "bad_options" {
		t.Fatalf("one option: %d %q", res.StatusCode, h.errorCode(res))
	}
	q := h.must(h.do(owner, "POST", base+"/questions", `{"label":"Meal","kind":"select","scope":"ticket","required":true,"options":["Beef","Veg"]}`), 201)
	id := q["id"].(string)
	if q["position"] != float64(1) || q["required"] != true || q["applies_to_all"] != true || len(q["options"].([]any)) != 2 {
		t.Fatalf("created: %v", q)
	}
	if res := h.do(owner, "PATCH", base+"/questions/"+id, `{"kind":"checkbox"}`); res.StatusCode != 200 {
		t.Fatalf("away from select: %d", res.StatusCode)
	}
	if res := h.do(owner, "PATCH", base+"/questions/"+id, `{"kind":"select"}`); res.StatusCode != 422 || h.errorCode(res) != "options_required_with_kind" {
		t.Fatalf("select alone: %d %q", res.StatusCode, h.errorCode(res))
	}
	if list := h.do(owner, "GET", base+"/questions", ""); list.StatusCode != 200 {
		t.Fatalf("list: %d", list.StatusCode)
	}
	h.must(h.do(owner, "GET", base+"/questions/"+id, ""), 200)
	h.must(h.do(owner, "DELETE", base+"/questions/"+id, ""), 204)
	h.must(h.do(owner, "GET", base+"/questions/"+id, ""), 404)
}

func TestReorderOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	_, readKey := h.createKey(owner, "acme", "read")
	_, writeKey := h.createKey(owner, "acme", "write")
	base := "/v1/organizers/acme/events/" + h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	for _, c := range []struct{ list, make1, make2 string }{
		{"ticket-types", `{"name":"General","price_cents":2500}`, `{"name":"VIP","price_cents":9000}`},
		{"questions", `{"label":"A","kind":"text","scope":"order"}`, `{"label":"B","kind":"text","scope":"order"}`},
	} {
		a := h.must(h.do(owner, "POST", base+"/"+c.list, c.make1), 201)["id"].(string)
		b := h.must(h.do(owner, "POST", base+"/"+c.list, c.make2), 201)["id"].(string)
		path, flipped := base+"/"+c.list+"/order", `{"ids":["`+b+`","`+a+`"]}`

		if res := h.do(readKey, "PUT", path, flipped); res.StatusCode != 403 || h.errorCode(res) != "scope_read_only" {
			t.Errorf("%s: read key: %d %q, want 403 scope_read_only", c.list, res.StatusCode, h.errorCode(res))
		}
		if res := h.do(owner, "PUT", path, `{"ids":["`+a+`"]}`); res.StatusCode != 422 || h.errorCode(res) != "order_mismatch" {
			t.Errorf("%s: short list: %d %q, want 422 order_mismatch", c.list, res.StatusCode, h.errorCode(res))
		}
		if res := h.do(owner, "PUT", path, `{}`); res.StatusCode != 422 || h.errorCode(res) != "order_mismatch" {
			t.Errorf("%s: no ids: %d %q, want 422 order_mismatch", c.list, res.StatusCode, h.errorCode(res))
		}
		res := h.do(writeKey, "PUT", path, flipped)
		var got []map[string]any
		_ = json.NewDecoder(res.Body).Decode(&got)
		if res.StatusCode != 200 || len(got) != 2 || got[0]["id"] != b || got[0]["position"] != float64(1) || got[1]["id"] != a || got[1]["position"] != float64(2) {
			t.Fatalf("%s: write key reorder: %d %v", c.list, res.StatusCode, got)
		}
		var list []map[string]any
		_ = json.NewDecoder(h.do(owner, "GET", base+"/"+c.list, "").Body).Decode(&list)
		if len(list) != 2 || list[0]["id"] != b {
			t.Errorf("%s: list after reorder: %v", c.list, list)
		}
	}
	// Another organizer's event is a 404, not a mismatch.
	h.createOrganizer(owner, "beta")
	if res := h.do(owner, "PUT", strings.Replace(base, "acme", "beta", 1)+"/ticket-types/order", `{"ids":[]}`); res.StatusCode != 404 {
		t.Errorf("wrong organizer: %d, want 404", res.StatusCode)
	}
}

func TestCreateEventNeedsBothTimes(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	stranger := h.signupWithMFA("stranger@example.com")
	h.createOrganizer(owner, "acme")
	for field, body := range map[string]string{
		"starts_at": `{"slug":"gala","name":"Gala","timezone":"Europe/London","ends_at":"2027-03-01T22:00:00Z"}`,
		"ends_at":   `{"slug":"gala","name":"Gala","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z"}`,
	} {
		res := h.do(owner, "POST", "/v1/organizers/acme/events", body)
		b := h.json(res)
		e, _ := b["error"].(map[string]any)
		if msg, _ := e["message"].(string); res.StatusCode != 422 || e["code"] != "missing_field" || !strings.Contains(msg, field) {
			t.Errorf("no %s: %d %v", field, res.StatusCode, b)
		}
		if res := h.do(stranger, "POST", "/v1/organizers/acme/events", body); res.StatusCode != 404 {
			t.Errorf("stranger, no %s: %d, want 404", field, res.StatusCode)
		}
	}
}

func TestGoLiveNeedsAnEventThatHasNotEnded(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	base := "/v1/organizers/acme/events/" + h.must(h.do(owner, "POST", "/v1/organizers/acme/events",
		`{"slug":"last-year","name":"Last Year","timezone":"Europe/London","starts_at":"2020-03-01T19:00:00Z","ends_at":"2020-03-01T22:00:00Z"}`), 201)["id"].(string)
	tt := h.must(h.do(owner, "POST", base+"/ticket-types", `{"name":"General","price_cents":2500}`), 201)["id"].(string)
	h.must(h.do(owner, "POST", base+"/quotas", `{"name":"Venue","size":100,"ticket_type_ids":["`+tt+`"]}`), 201)

	res := h.do(owner, "PATCH", base, `{"status":"live"}`)
	e, _ := h.json(res)["error"].(map[string]any)
	if res.StatusCode != 422 || e["code"] != "event_over" || e["message"] != "This event has already ended, so it can't go on sale." {
		t.Fatalf("ended event: %d %v", res.StatusCode, e)
	}

	// Started an hour ago and still running: that can go on sale.
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	h.must(h.do(owner, "PATCH", base, `{"starts_at":"`+at(-time.Hour)+`","ends_at":"`+at(2*time.Hour)+`"}`), 200)
	h.chargesEnabled("acme")
	h.must(h.do(owner, "PATCH", base, `{"status":"live"}`), 200)
	// Once live, the end time can still move, even into the past.
	if up := h.must(h.do(owner, "PATCH", base, `{"ends_at":"`+at(-30*time.Minute)+`"}`), 200); up["status"] != "live" {
		t.Fatalf("after moving the end: %v", up)
	}
}

func TestQuestionPatchIgnoresPosition(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	base := "/v1/organizers/acme/events/" + h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	a := h.must(h.do(owner, "POST", base+"/questions", `{"label":"A","kind":"text","scope":"order"}`), 201)["id"].(string)
	h.must(h.do(owner, "POST", base+"/questions", `{"label":"B","kind":"text","scope":"order"}`), 201)
	// Order changes only through the order endpoint.
	if q := h.must(h.do(owner, "PATCH", base+"/questions/"+a, `{"label":"A2","position":2}`), 200); q["position"] != float64(1) || q["label"] != "A2" {
		t.Fatalf("patched: %v", q)
	}
}

// Once a code is used or a question answered, the deletes answer 409
// has_sales; the catalogue tests cover the rest of the guards.
func TestSalesGuardsOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	ev := h.must(h.do(owner, "POST", "/v1/organizers/acme/events", eventBody), 201)["id"].(string)
	base := "/v1/organizers/acme/events/" + ev
	promo := h.must(h.do(owner, "POST", base+"/promo-codes", `{"code":"EARLY","kind":"percent","value":1000}`), 201)["id"].(string)
	question := h.must(h.do(owner, "POST", base+"/questions", `{"label":"Diet","kind":"text","scope":"order"}`), 201)["id"].(string)
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `with u as (update promo_codes set used_count = 1 where id = $2),
		o as (insert into orders(organizer_id, event_id, code, email, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details)
			select organizer_id, id, 'TS-AAAAA', 'b@example.com', 0, 0, 0, 0, 0, 'pass', '{}', '{}' from events where id = $1 returning id, organizer_id, event_id)
		insert into answers(organizer_id, event_id, order_id, question_id, value) select organizer_id, event_id, id, $3, 'None' from o`,
		ev, promo, question); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base + "/promo-codes/" + promo, base + "/questions/" + question} {
		if res := h.do(owner, "DELETE", path, ""); res.StatusCode != 409 || h.errorCode(res) != "has_sales" {
			t.Errorf("delete %s: %d %q", path, res.StatusCode, h.errorCode(res))
		}
	}
	if res := h.do(owner, "PATCH", base+"/promo-codes/"+promo, `{"value":2000}`); res.StatusCode != 409 || h.errorCode(res) != "has_sales" {
		t.Errorf("patch value: %d %q", res.StatusCode, h.errorCode(res))
	}
}
