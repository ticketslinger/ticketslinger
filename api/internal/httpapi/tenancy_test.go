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

// Every secured operation not in userScoped, called by a signed-in mfa user
// against another tenant's real resources, must answer 404. A new operation
// is checked automatically; one with a path parameter the fixture can't fill
// fails until the fixture seeds it.
func TestEverySecuredOperationHidesOtherTenants(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	stranger := h.signupWithMFA("stranger@example.com")
	// The other tenant's real resources, by path parameter name.
	fixture := map[string]string{"organizer": "acme"}

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
			if _, ok := userScoped[id]; ok {
				continue
			}
			url := param.ReplaceAllStringFunc(path, func(m string) string {
				v, ok := fixture[m[1:len(m)-1]]
				if !ok {
					t.Errorf("%s: no fixture for path parameter %s; seed another tenant's real one", id, m)
				}
				return v
			})
			res := h.do(stranger, method, "/v1"+url, `{}`)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s (%s) as non-member: got %d, want 404", method, url, id, res.StatusCode)
			}
			checked++
		}
	}
	for id := range userScoped {
		if !seen[id] {
			t.Errorf("userScoped lists %s, which isn't in the spec", id)
		}
	}
	if checked == 0 {
		t.Fatal("no tenant operations checked; the walk is broken")
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
