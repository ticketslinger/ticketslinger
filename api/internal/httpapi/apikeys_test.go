package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAPIKeyActsOnlyWithinItsOrganizer(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	other := h.signupWithMFA("other@example.com")
	h.createOrganizer(other, "beta")
	id, key := h.createKey(owner, "acme", "write")

	b := h.must(h.do(key, "GET", "/v1/organizers/acme", ""), 200)
	if b["slug"] != "acme" || b["role"] != "key:write" {
		t.Fatalf("own organizer as key: %v", b)
	}
	h.must(h.do(key, "GET", "/v1/organizers/beta", ""), 404)
	h.must(h.do(key, "GET", "/v1/organizers/nope", ""), 404)

	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/me", ""},
		{"GET", "/v1/organizers", ""},
		{"POST", "/v1/organizers", `{"name":"X","slug":"xyz"}`},
		{"POST", "/v1/auth/totp", `{}`},
		{"POST", "/v1/auth/logout", ""},
		{"GET", "/v1/organizers/acme/audit", ""},
		{"GET", "/v1/organizers/acme/api-keys", ""},
		{"POST", "/v1/organizers/acme/api-keys", `{"name":"again","scope":"write"}`},
		{"DELETE", "/v1/organizers/acme/api-keys/" + id, ""},
	} {
		res := h.do(key, c.method, c.path, c.body)
		if res.StatusCode != http.StatusForbidden || h.errorCode(res) != "session_required" {
			t.Errorf("%s %s as key: got %d %q, want 403 session_required", c.method, c.path, res.StatusCode, h.errorCode(res))
		}
	}

	// The owner sees the key without its secret, then revokes it.
	res := h.do(owner, "GET", "/v1/organizers/acme/api-keys", "")
	var keys []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&keys)
	if res.StatusCode != 200 || len(keys) != 1 || keys[0]["id"] != id || keys[0]["prefix"] != key[4:12] || keys[0]["token"] != nil {
		t.Fatalf("list: %d %v", res.StatusCode, keys)
	}
	h.must(h.do(owner, "DELETE", "/v1/organizers/acme/api-keys/"+id, ""), 204)
	if res := h.do(key, "GET", "/v1/organizers/acme", ""); res.StatusCode != 401 {
		t.Fatalf("revoked key: got %d, want 401", res.StatusCode)
	}
	h.must(h.do(owner, "DELETE", "/v1/organizers/acme/api-keys/0190a0a0-0000-7000-8000-000000000000", ""), 404)

	res = h.do(owner, "GET", "/v1/organizers/acme/audit", "")
	var rows []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&rows)
	if len(rows) < 2 || rows[0]["action"] != "api_key.revoked" || rows[1]["action"] != "api_key.created" || rows[0]["actor_user_id"] == nil {
		t.Fatalf("audit: %v", rows)
	}
}

func TestCreateAPIKeyValidation(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	for _, c := range []struct{ body, code string }{
		{`{"name":"CI","scope":"admin"}`, "invalid_scope"},
		{`{"name":"  ","scope":"read"}`, "invalid_name"},
	} {
		res := h.do(owner, "POST", "/v1/organizers/acme/api-keys", c.body)
		if res.StatusCode != 422 || h.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q, want 422 %s", c.body, res.StatusCode, h.errorCode(res), c.code)
		}
	}
	plain := h.signupVerified("plain@example.com")
	h.createOrganizer(plain, "plain")
	if res := h.do(plain, "POST", "/v1/organizers/plain/api-keys", `{"name":"CI","scope":"read"}`); res.StatusCode != 403 || h.errorCode(res) != "mfa_required" {
		t.Fatalf("non-mfa owner: got %d %q, want 403 mfa_required", res.StatusCode, h.errorCode(res))
	}
}

func TestUnknownAPIKeyIsUnauthorized(t *testing.T) {
	h := newHarness(t)
	res := h.do("tsk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "GET", "/v1/organizers/acme", "")
	if res.StatusCode != 401 {
		t.Fatalf("got %d, want 401", res.StatusCode)
	}
}
