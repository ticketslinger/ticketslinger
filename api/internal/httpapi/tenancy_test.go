package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
)

// Every route under /organizers/{organizer} must answer 404 to a signed-in
// non-member, whatever the method. New routes are covered automatically.
func TestEveryOrganizerRouteHidesOtherTenants(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	stranger := h.signupWithMFA("stranger@example.com")

	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/organizers/{organizer}") {
			continue
		}
		for method := range item.Operations() {
			url := strings.ReplaceAll(path, "{organizer}", "acme")
			url = strings.NewReplacer("{event}", "x", "{id}", "00000000-0000-0000-0000-000000000000").Replace(url)
			res := h.do(stranger, method, "/v1"+url, `{}`)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s as non-member: got %d, want 404", method, path, res.StatusCode)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no organizer routes found; the walk is broken")
	}
}

func TestAdminRoutesNeedMFA(t *testing.T) {
	h := newHarness(t)
	// Verified but no passkey or authenticator yet: session.mfa = false.
	plain := h.signupVerified("owner@example.com")
	h.createOrganizer(plain, "acme") // creating is allowed without MFA
	res := h.do(plain, "GET", "/v1/organizers/acme/audit", "")
	if res.StatusCode != http.StatusForbidden || h.errorCode(res) != "mfa_required" {
		t.Fatalf("got %d %q, want 403 mfa_required", res.StatusCode, h.errorCode(res))
	}
}

func TestOrganizerEndpoints(t *testing.T) {
	h := newHarness(t)
	if res := h.do(h.signupUnverified("new@example.com"), "POST", "/v1/organizers", `{"name":"X","slug":"xyz"}`); res.StatusCode != 403 || h.errorCode(res) != "email_unverified" {
		t.Fatalf("unverified create: %d", res.StatusCode)
	}
	tok := h.signupWithMFA("owner@example.com")
	h.createOrganizer(tok, "acme")
	if res := h.do(tok, "POST", "/v1/organizers", `{"name":"Again","slug":"acme"}`); res.StatusCode != 409 || h.errorCode(res) != "slug_taken" {
		t.Fatalf("duplicate: %d", res.StatusCode)
	}
	if l := h.do(tok, "GET", "/v1/organizers", ""); l.StatusCode != 200 {
		t.Fatalf("list: %d", l.StatusCode)
	}
	h.must(h.do(tok, "GET", "/v1/organizers/acme", ""), 200)
	res := h.do(tok, "GET", "/v1/organizers/acme/audit", "")
	if res.StatusCode != 200 {
		t.Fatalf("audit: %d", res.StatusCode)
	}
	var rows []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&rows)
	if len(rows) != 1 || rows[0]["action"] != "organizer.created" {
		t.Fatalf("audit rows: %v", rows)
	}
}
