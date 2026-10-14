package httpapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func TestAuditNamesTheActor(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	staff := h.signupWithMFA("staff@example.com")
	root := testdb.Owner(t, h.pool)
	exec := func(q string) {
		t.Helper()
		if _, err := root.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE users SET name = 'Olive Owner' WHERE email = 'owner@example.com'`)
	exec(`UPDATE users SET name = 'Sam Staff' WHERE email = 'staff@example.com'`)
	exec(`INSERT INTO memberships (organizer_id, user_id, role)
		SELECT o.id, u.id, 'admin' FROM organizers o, users u WHERE o.slug = 'acme' AND u.email = 'staff@example.com'`)

	_, key := h.createKey(owner, "acme", "write")
	event := func(tok, slug string) {
		t.Helper()
		h.must(h.do(tok, "POST", "/v1/organizers/acme/events", strings.Replace(eventBody, "spring-gala", slug, 1)), 201)
	}
	event(owner, "by-owner")
	event(staff, "by-staff")
	event(key, "by-key")

	// Newest first: the key's event, the staff's, the owner's.
	actors := func() []map[string]any {
		t.Helper()
		res := h.do(owner, "GET", "/v1/organizers/acme/audit", "")
		var rows []map[string]any
		var all []map[string]any
		_ = json.NewDecoder(res.Body).Decode(&all)
		for _, r := range all {
			if r["action"] == "event.created" {
				rows = append(rows, r)
			}
		}
		if len(rows) != 3 {
			t.Fatalf("event.created rows: %v", rows)
		}
		return rows
	}
	check := func(r map[string]any, name, email any) {
		t.Helper()
		n, nok := r["actor_name"]
		e, eok := r["actor_email"]
		if !nok || !eok || n != name || e != email {
			t.Errorf("actor = (%v, %v) present=(%v, %v), want (%v, %v)", n, e, nok, eok, name, email)
		}
	}
	rows := actors()
	check(rows[0], "CI", nil)
	check(rows[1], "Sam Staff", "staff@example.com")
	check(rows[2], "Olive Owner", "owner@example.com")

	// A revoked key keeps its name; a former member loses theirs.
	keyID := rows[0]["actor_api_key_id"].(string)
	h.must(h.do(owner, "DELETE", "/v1/organizers/acme/api-keys/"+keyID, ""), 204)
	exec(`DELETE FROM memberships WHERE user_id = (SELECT id FROM users WHERE email = 'staff@example.com')`)
	rows = actors()
	check(rows[0], "CI", nil)
	check(rows[1], nil, nil)
	check(rows[2], "Olive Owner", "owner@example.com")
}
