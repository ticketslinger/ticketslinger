package httpapi_test

import (
	"strings"
	"testing"
)

// Every 422 that is about one property names it, and the ones that aren't
// leave field out. One table per area.
func TestValidationErrorsNameTheField(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	f := seedCatalogue(h, owner, "acme", "")
	org := "/v1/organizers/acme"
	ev := org + "/events/" + f["event"]
	// A hidden ticket type, a ticket type of another event, and a second ticket type to reorder.
	hidden := h.must(h.do(owner, "POST", ev+"/ticket-types", `{"name":"Secret","price_cents":100,"hidden":true}`), 201)["id"].(string)
	other := h.must(h.do(owner, "POST", org+"/events", `{"slug":"other","name":"Other","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`), 201)["id"].(string)
	foreign := h.must(h.do(owner, "POST", org+"/events/"+other+"/ticket-types", `{"name":"Elsewhere","price_cents":1}`), 201)["id"].(string)
	long := func(n int) string { return strings.Repeat("a", n) }

	type c struct{ name, method, path, body, code, field string }
	areas := map[string][]c{
		"events": {
			{"slug", "PATCH", ev, `{"slug":"X!"}`, "invalid_slug", "slug"},
			{"name", "PATCH", ev, `{"name":"` + long(161) + `"}`, "invalid_name", "name"},
			{"description", "PATCH", ev, `{"description":"` + long(20001) + `"}`, "invalid_description", "description"},
			{"venue name", "PATCH", ev, `{"venue_name":"` + long(161) + `"}`, "invalid_venue", "venue_name"},
			{"venue address", "PATCH", ev, `{"venue_address":"` + long(501) + `"}`, "invalid_venue", "venue_address"},
			{"timezone", "PATCH", ev, `{"timezone":"Mars/Base"}`, "bad_timezone", "timezone"},
			{"times", "PATCH", ev, `{"ends_at":"2027-03-01T18:00:00Z"}`, "bad_times", "ends_at"},
			{"tax", "PATCH", ev, `{"tax_rate_bp":10001}`, "invalid_tax_rate", "tax_rate_bp"},
			{"fee mode", "PATCH", ev, `{"fee_mode":"split"}`, "invalid_fee_mode", "fee_mode"},
			{"status", "PATCH", ev, `{"status":"ended"}`, "status_not_allowed", "status"},
			{"missing starts_at", "POST", org + "/events", `{"slug":"new","name":"N","timezone":"Europe/London","ends_at":"2027-03-01T22:00:00Z"}`, "missing_field", "starts_at"},
		},
		"ticket types": {
			{"name", "POST", ev + "/ticket-types", `{"name":" ","price_cents":1}`, "invalid_name", "name"},
			{"description", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"description":"` + long(2001) + `"}`, "invalid_description", "description"},
			{"price", "POST", ev + "/ticket-types", `{"name":"X","price_cents":-5}`, "bad_price", "price_cents"},
			{"tax", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"tax_rate_bp":10001}`, "invalid_tax_rate", "tax_rate_bp"},
			{"min limit", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"min_per_order":0}`, "bad_limits", "min_per_order"},
			{"max limit", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"max_per_order":101}`, "bad_limits", "max_per_order"},
			{"min above max", "PATCH", ev + "/ticket-types/" + f["ticketType"], `{"min_per_order":50}`, "bad_limits", "max_per_order"},
			{"sales window", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"sales_start":"2027-01-02T00:00:00Z","sales_end":"2027-01-01T00:00:00Z"}`, "bad_sales_window", "sales_end"},
			{"quota", "POST", ev + "/ticket-types", `{"name":"X","price_cents":1,"quota_ids":["` + foreign + `"]}`, "unknown_quota", "quota_ids"},
			{"missing price", "POST", ev + "/ticket-types", `{"name":"X"}`, "missing_field", "price_cents"},
		},
		"quotas": {
			{"name", "POST", ev + "/quotas", `{"name":"","size":1}`, "invalid_name", "name"},
			{"size", "POST", ev + "/quotas", `{"name":"Q","size":-1}`, "bad_quota_size", "size"},
			{"ticket types", "POST", ev + "/quotas", `{"name":"Q","size":1,"ticket_type_ids":["` + foreign + `"]}`, "unknown_ticket_type", "ticket_type_ids"},
			{"missing size", "POST", ev + "/quotas", `{"name":"Q"}`, "missing_field", "size"},
		},
		"promo codes": {
			{"code", "POST", ev + "/promo-codes", `{"code":"x","kind":"percent","value":10}`, "bad_code", "code"},
			{"kind", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"free","value":10}`, "bad_kind", "kind"},
			{"percent", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10001}`, "bad_discount", "value"},
			{"no discount", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":0}`, "bad_discount", "value"},
			{"max uses", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"max_uses":0}`, "bad_max_uses", "max_uses"},
			{"validity", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"valid_from":"2027-01-02T00:00:00Z","valid_until":"2027-01-01T00:00:00Z"}`, "bad_validity", "valid_until"},
			{"conflict", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"applies_to":["` + f["ticketType"] + `"]}`, "applies_to_conflict", "applies_to"},
			{"required", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"applies_to_all":false}`, "applies_to_required", "applies_to"},
			{"applies to foreign", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"applies_to_all":false,"applies_to":["` + foreign + `"]}`, "unknown_ticket_type", "applies_to"},
			{"unlock foreign", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"unlocks":["` + foreign + `"]}`, "unknown_ticket_type", "unlocks"},
			{"unlock visible", "POST", ev + "/promo-codes", `{"code":"GOOD","kind":"percent","value":10,"unlocks":["` + f["ticketType"] + `"]}`, "unlock_not_hidden", "unlocks"},
			{"kind alone", "PATCH", ev + "/promo-codes/" + f["promoCode"], `{"kind":"amount"}`, "value_required_with_kind", "value"},
			{"missing code", "POST", ev + "/promo-codes", `{"kind":"percent","value":10}`, "missing_field", "code"},
		},
		"questions": {
			{"label", "POST", ev + "/questions", `{"label":"","kind":"text","scope":"order"}`, "bad_label", "label"},
			{"kind", "POST", ev + "/questions", `{"label":"Q","kind":"radio","scope":"order"}`, "bad_kind", "kind"},
			{"scope", "POST", ev + "/questions", `{"label":"Q","kind":"text","scope":"event"}`, "bad_scope", "scope"},
			{"options on text", "POST", ev + "/questions", `{"label":"Q","kind":"text","scope":"order","options":["a","b"]}`, "bad_options", "options"},
			{"too few options", "POST", ev + "/questions", `{"label":"Q","kind":"select","scope":"order","options":["a"]}`, "bad_options", "options"},
			{"empty option", "POST", ev + "/questions", `{"label":"Q","kind":"select","scope":"order","options":["a","b"," "]}`, "bad_options", "options[2]"},
			{"repeated option", "POST", ev + "/questions", `{"label":"Q","kind":"select","scope":"order","options":["a","b","A"]}`, "bad_options", "options[2]"},
			{"applies to", "POST", ev + "/questions", `{"label":"Q","kind":"text","scope":"ticket","applies_to_all":false,"applies_to":["` + foreign + `"]}`, "unknown_ticket_type", "applies_to"},
			{"select alone", "PATCH", ev + "/questions/" + f["question"], `{"kind":"select"}`, "options_required_with_kind", "options"},
			{"missing scope", "POST", ev + "/questions", `{"label":"Q","kind":"text"}`, "missing_field", "scope"},
		},
		"reorder": {
			{"ids", "PUT", ev + "/ticket-types/order", `{"ids":["` + hidden + `"]}`, "order_mismatch", "ids"},
		},
		"accounts": {
			{"password", "POST", "/v1/auth/signup", `{"email":"b@example.com","password":"short","name":"B"}`, "password_too_short", "password"},
			{"name", "POST", "/v1/auth/signup", `{"email":"b@example.com","password":"a long passphrase","name":" "}`, "invalid_name", "name"},
			{"recovery code", "POST", "/v1/auth/login", `{"email":"b@example.com","password":"x","recovery_code":"` + long(70) + `"}`, "invalid_recovery_code", "recovery_code"},
			{"organizer name", "POST", "/v1/organizers", `{"name":" ","slug":"fine-slug"}`, "invalid_name", "name"},
			{"organizer slug", "POST", "/v1/organizers", `{"name":"Fine","slug":"A b"}`, "invalid_slug", "slug"},
			{"key name", "POST", org + "/api-keys", `{"name":" ","scope":"read"}`, "invalid_name", "name"},
			{"key scope", "POST", org + "/api-keys", `{"name":"CI","scope":"admin"}`, "invalid_scope", "scope"},
		},
	}
	for area, cases := range areas {
		for _, k := range cases {
			token := owner
			if strings.HasPrefix(k.path, "/v1/auth/") {
				token = ""
			}
			res := h.do(token, k.method, k.path, k.body)
			e, _ := h.json(res)["error"].(map[string]any)
			code, _ := e["code"].(string)
			field, _ := e["field"].(string)
			if res.StatusCode != 422 || code != k.code || field != k.field {
				t.Errorf("%s/%s: got %d %q field %q, want 422 %q field %q", area, k.name, res.StatusCode, code, field, k.code, k.field)
			}
		}
	}

	// Clashes name their property too.
	for _, k := range []c{
		{"event slug", "POST", org + "/events", `{"slug":"gala","name":"G","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`, "slug_taken", "slug"},
		{"promo code", "POST", ev + "/promo-codes", `{"code":"early","kind":"percent","value":10}`, "code_taken", "code"},
		{"organizer slug", "POST", "/v1/organizers", `{"name":"Again","slug":"acme"}`, "slug_taken", "slug"},
		{"email", "POST", "/v1/auth/signup", `{"email":"owner@example.com","password":"a long passphrase","name":"O"}`, "email_taken", "email"},
	} {
		token := owner
		if strings.HasPrefix(k.path, "/v1/auth/") {
			token = ""
		}
		res := h.do(token, k.method, k.path, k.body)
		if e, _ := h.json(res)["error"].(map[string]any); res.StatusCode != 409 || e["code"] != k.code || e["field"] != k.field {
			t.Errorf("%s: got %d %v, want 409 %s field %q", k.name, res.StatusCode, e, k.code, k.field)
		}
	}

	// An error that isn't about one property leaves field out.
	for _, k := range []c{
		{"two second steps", "POST", "/v1/auth/login", `{"email":"b@example.com","password":"x","totp_code":"123456","recovery_code":"aaaa-bbbb"}`, "one_second_step", ""},
		{"live without capacity", "PATCH", org + "/events/" + other, `{"status":"live"}`, "unquotaed_ticket_type", "status"},
	} {
		res := h.do("", k.method, k.path, k.body)
		if k.name == "live without capacity" {
			res = h.do(owner, k.method, k.path, k.body)
		}
		if e, _ := h.json(res)["error"].(map[string]any); res.StatusCode != 422 || e["code"] != k.code || e["field"] != nilIfEmpty(k.field) {
			t.Errorf("%s: got %d %v, want 422 %s field %q", k.name, res.StatusCode, e, k.code, k.field)
		}
	}
	if res := h.do(owner, "GET", ev+"/quotas/00000000-0000-7000-8000-000000000000", ""); res.StatusCode != 404 {
		t.Fatalf("404 setup: %d", res.StatusCode)
	} else if e, _ := h.json(res)["error"].(map[string]any); e["field"] != nil {
		t.Errorf("a 404 names a field: %v", e)
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
