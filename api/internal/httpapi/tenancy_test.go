package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// userScoped lists the secured operations that act on the caller's own
// account rather than on a tenant's data, so a non-member has nothing to be
// refused. Every other secured operation must answer 404 to a signed-in
// stranger. Keep this list short, and give each entry a reason.
var userScoped = map[string]string{
	"logout":                    "ends the caller's own session",
	"getMe":                     "returns the caller's own account",
	"listOrganizers":            "lists only the organizers the caller belongs to",
	"createOrganizer":           "creates a new organizer owned by the caller",
	"startTotp":                 "enrols an authenticator on the caller's own account",
	"confirmTotp":               "confirms the caller's own authenticator",
	"createRecoveryCodes":       "replaces the caller's own recovery codes",
	"beginPasskeyRegistration":  "starts a passkey on the caller's own account",
	"finishPasskeyRegistration": "finishes a passkey on the caller's own account",
}

// opID is the operationId as openapi.yaml spells it; the embedded spec
// capitalises the first letter.
func opID(op *openapi3.Operation) string {
	return strings.ToLower(op.OperationID[:1]) + op.OperationID[1:]
}

// sessionOnly lists tenant operations an API key may never call, even for
// its own organizer: they need a person behind a signed-in session.
var sessionOnly = map[string]string{
	"listAudit":    "the audit log is for people, not integrations, in this release",
	"listApiKeys":  "keys don't manage keys",
	"createApiKey": "keys don't mint keys",
	"revokeApiKey": "keys don't revoke keys",
}

// Every secured operation not in userScoped, called by a signed-in mfa user
// against another tenant's real resources, must answer 404. A new operation
// is checked automatically; one with a path parameter the fixture can't fill
// fails until the fixture seeds it.
//
// The walk runs again as a write API key of a third organizer: tenant
// operations answer 404 as for a stranger, and anything that needs a person
// (userScoped, sessionOnly) answers 403 session_required.
func TestEverySecuredOperationHidesOtherTenants(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	acmeKey, _ := h.createKey(owner, "acme", "write")
	stranger := h.signupWithMFA("stranger@example.com")
	third := h.signupWithMFA("third@example.com")
	h.createOrganizer(third, "third")
	_, thirdKey := h.createKey(third, "third", "write")
	event := h.must(h.do(owner, "POST", "/v1/organizers/acme/events",
		`{"slug":"gala","name":"Gala","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`), 201)["id"].(string)
	// The other tenant's real resources, by path parameter name.
	// challengeId only appears on a user-scoped operation, which the key run
	// still calls; any well-formed id will do.
	fixture := map[string]string{"organizer": "acme", "apiKey": acmeKey, "event": event, "challengeId": "0190a0a0-0000-7000-8000-000000000000"}

	t.Run("stranger", func(t *testing.T) {
		walk(t, h, stranger, fixture, func(id string) (int, string) {
			if _, ok := userScoped[id]; ok {
				return 0, "" // skipped
			}
			return http.StatusNotFound, ""
		})
	})
	t.Run("key", func(t *testing.T) {
		walk(t, h, thirdKey, fixture, func(id string) (int, string) {
			_, user := userScoped[id]
			_, sess := sessionOnly[id]
			if user || sess {
				return http.StatusForbidden, "session_required"
			}
			return http.StatusNotFound, ""
		})
	})
}

// walk calls every secured operation with token and checks the status (and
// error code, if want gives one); a zero status skips the operation.
func walk(t *testing.T, h *harness, token string, fixture map[string]string, want func(id string) (int, string)) {
	defer func(old *testing.T) { h.t = old }(h.t)
	h.t = t
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{([^}]*)\}`)
	seen := map[string]bool{}
	checked := 0
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			id := opID(op)
			seen[id] = true
			if op.Security != nil && len(*op.Security) == 0 {
				continue // public; TestPublicOperationsAreExactlyThese pins the set
			}
			status, code := want(id)
			if status == 0 {
				continue
			}
			url := param.ReplaceAllStringFunc(path, func(m string) string {
				v, ok := fixture[m[1:len(m)-1]]
				if !ok {
					t.Errorf("%s: no fixture for path parameter %s; seed another tenant's real one", id, m)
				}
				return v
			})
			res := h.do(token, method, "/v1"+url, `{}`)
			if got := h.errorCode(res); res.StatusCode != status || (code != "" && got != code) {
				t.Errorf("%s %s (%s): got %d %q, want %d %q", method, url, id, res.StatusCode, got, status, code)
			}
			checked++
		}
	}
	for _, list := range []map[string]string{userScoped, sessionOnly} {
		for id := range list {
			if !seen[id] {
				t.Errorf("%s is listed but isn't in the spec", id)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operations checked; the walk is broken")
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

// A member whose role isn't allowed gets 403 forbidden_role, not 404: they
// already know the organizer exists.
func TestCheckinMemberCannotReadAudit(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	staff := h.signupWithMFA("staff@example.com")
	_, err := testdb.Owner(t, h.pool).Exec(context.Background(),
		`INSERT INTO memberships (organizer_id, user_id, role)
		 SELECT o.id, u.id, 'checkin' FROM organizers o, users u
		  WHERE o.slug = 'acme' AND u.email = 'staff@example.com'`)
	if err != nil {
		t.Fatal(err)
	}
	h.must(h.do(staff, "GET", "/v1/organizers/acme", ""), 200)
	res := h.do(staff, "GET", "/v1/organizers/acme/audit", "")
	if res.StatusCode != http.StatusForbidden || h.errorCode(res) != "forbidden_role" {
		t.Fatalf("checkin reading audit: got %d %q, want 403 forbidden_role", res.StatusCode, h.errorCode(res))
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

func TestCreateOrganizerValidation(t *testing.T) {
	h := newHarness(t)
	tok := h.signupVerified("owner@example.com")
	cases := []struct{ name, name_, slug, code string }{
		{"uppercase", "A", "ACME", "invalid_slug"},
		{"mixed case", "A", "Acme-Co", "invalid_slug"},
		{"too short", "A", "ab", "invalid_slug"},
		{"too long", "A", strings.Repeat("a", 41), "invalid_slug"},
		{"leading hyphen", "A", "-abc", "invalid_slug"},
		{"trailing hyphen", "A", "abc-", "invalid_slug"},
		{"double hyphen", "A", "a--bc", "invalid_slug"},
		{"empty name", "  ", "acme", "invalid_name"},
		{"long name", strings.Repeat("n", 121), "acme", "invalid_name"},
	}
	for _, c := range cases {
		res := h.do(tok, "POST", "/v1/organizers", `{"name":"`+c.name_+`","slug":"`+c.slug+`"}`)
		if res.StatusCode != 422 || h.errorCode(res) != c.code {
			t.Errorf("%s: got %d %q, want 422 %s", c.name, res.StatusCode, h.errorCode(res), c.code)
		}
	}
}
