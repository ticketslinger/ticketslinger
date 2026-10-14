package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (h *harness) doKeyed(token, method, path, key, body string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestIdempotentCreateEvent(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	a, abody := h.doKeyed(owner, "POST", "/v1/organizers/acme/events", "key-1", eventBody)
	b, bbody := h.doKeyed(owner, "POST", "/v1/organizers/acme/events", "key-1", eventBody)
	if a.StatusCode != 201 || b.StatusCode != 201 || abody != bbody || b.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("first %d %s; second %d %s replayed=%q", a.StatusCode, abody, b.StatusCode, bbody, b.Header.Get("Idempotent-Replayed"))
	}
	var keys int
	_ = h.pool.QueryRow(context.Background(), `select count(*) from idempotency_keys`).Scan(&keys)
	if keys != 1 {
		t.Errorf("stored keys = %d, want 1", keys)
	}
	list := h.do(owner, "GET", "/v1/organizers/acme/events", "")
	body, _ := io.ReadAll(list.Body)
	if n := strings.Count(string(body), `"slug"`); n != 1 {
		t.Errorf("events listed = %d, want 1: %s", n, body)
	}
	au, _ := io.ReadAll(h.do(owner, "GET", "/v1/organizers/acme/audit", "").Body)
	if audits := strings.Count(string(au), `"event.created"`); audits != 1 {
		t.Errorf("event.created audit rows = %d, want 1", audits)
	}
}

func TestIdempotencyKeysAreScopedToTheCaller(t *testing.T) {
	h := newHarness(t)
	a := h.signupWithMFA("a@example.com")
	b := h.signupWithMFA("b@example.com")
	h.createOrganizer(a, "acme")
	h.createOrganizer(b, "other")
	first, _ := h.doKeyed(a, "POST", "/v1/organizers/acme/events", "shared", eventBody)
	if first.StatusCode != 201 {
		t.Fatalf("a: %d", first.StatusCode)
	}
	// b reuses a's key on a path b can't see: must not replay a's response.
	res, body := h.doKeyed(b, "POST", "/v1/organizers/acme/events", "shared", eventBody)
	if res.Header.Get("Idempotent-Replayed") != "" || res.StatusCode != 404 {
		t.Fatalf("b got %d replayed=%q %s", res.StatusCode, res.Header.Get("Idempotent-Replayed"), body)
	}
}

func TestBadKeyOverHTTP(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	res, body := h.doKeyed(owner, "POST", "/v1/organizers/acme/events", strings.Repeat("k", 256), eventBody)
	if res.StatusCode != 400 || !strings.Contains(body, "bad_idempotency_key") {
		t.Fatalf("got %d %s", res.StatusCode, body)
	}
}
