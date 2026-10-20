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
	"listPasskeys":              "lists only the caller's own passkeys",
	"beginPasskeyRegistration":  "starts a passkey on the caller's own account",
	"finishPasskeyRegistration": "finishes a passkey on the caller's own account",
	"completeStripeLink":        "the state names the organizer, and only the session that started it may use it; anyone else gets 400 invalid_state",
}

// opID is the operationId as openapi.yaml spells it; the embedded spec
// capitalises the first letter.
func opID(op *openapi3.Operation) string {
	return strings.ToLower(op.OperationID[:1]) + op.OperationID[1:]
}

// sessionOnly lists tenant operations an API key may never call, even for
// its own organizer: they need a person behind a signed-in session.
var sessionOnly = map[string]string{
	"listAudit":          "the audit log is for people, not integrations, in this release",
	"listApiKeys":        "keys don't manage keys",
	"createApiKey":       "keys don't mint keys",
	"revokeApiKey":       "keys don't revoke keys",
	"connectPayments":    "a person signs Stripe's terms, and the link is theirs to follow",
	"startStripeLink":    "a person picks the Stripe account, and the state is tied to their session",
	"disconnectPayments": "switching payments off is a person's call",
	"refundOrder":        "a refund moves the organizer's money; a person decides",
	"retryRefund":        "sending a refund again is a person's call too",
	"cancelEvent":        "cancelling an event refunds everyone; a person decides",
	"listMembers":        "who is on the team is for people, not integrations",
	"addMember":          "giving someone access to the organizer is a person's call",
	"removeMember":       "taking someone's access away is a person's call",
	"listStaffAccess":    "who from TicketSlinger looked at the organizer is for its owners, not integrations",
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
	fixture := seedCatalogue(h, owner, "acme", acmeKey)

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

// seedCatalogue gives organizer slug one of everything, as owner, and returns
// the ids by path parameter name. challengeId only appears on a user-scoped
// operation, which the key run still calls; any well-formed id will do.
func seedCatalogue(h *harness, owner, slug, apiKey string) map[string]string {
	base := "/v1/organizers/" + slug
	event := h.must(h.do(owner, "POST", base+"/events",
		`{"slug":"gala","name":"Gala","timezone":"Europe/London","starts_at":"2027-03-01T19:00:00Z","ends_at":"2027-03-01T22:00:00Z"}`), 201)["id"].(string)
	ticketType := h.must(h.do(owner, "POST", base+"/events/"+event+"/ticket-types", `{"name":"General","price_cents":2500}`), 201)["id"].(string)
	quota := h.must(h.do(owner, "POST", base+"/events/"+event+"/quotas", `{"name":"Hall","size":100,"ticket_type_ids":["`+ticketType+`"]}`), 201)["id"].(string)
	promoCode := h.must(h.do(owner, "POST", base+"/events/"+event+"/promo-codes", `{"code":"EARLY","kind":"percent","value":1000}`), 201)["id"].(string)
	question := h.must(h.do(owner, "POST", base+"/events/"+event+"/questions", `{"label":"Dietary needs","kind":"text","scope":"order"}`), 201)["id"].(string)
	// A paid order with no tickets left, and a refund of it that failed.
	var order, refund string
	if err := testdb.Owner(h.t, h.pool).QueryRow(context.Background(), `with o as (
			insert into orders (organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents,
				fee_mode, quote, details, paid_at)
			select organizer_id, id, 'TS-' || upper(substr(md5($2), 1, 5)), 'seed@example.com', 'paid', 2500, 0, 0, 100, 2600,
				'pass', '{}', '{}', now() from events where id = $1 returning id, organizer_id),
		p as (insert into payments (organizer_id, order_id, provider, stripe_account_id, payment_intent_id, client_secret, amount_cents,
				application_fee_cents, status)
			select organizer_id, id, 'test', 'acct_seed', 'pi_seed_' || $2, '', 2600, 100, 'succeeded' from o returning id, organizer_id),
		r as (insert into refunds (organizer_id, payment_id, amount_cents, fee_refunded, status, reason, failure)
			select organizer_id, id, 100, false, 'failed', 'organizer', 'gave_up' from p returning id)
		select o.id::text, r.id::text from o, r`, event, slug).Scan(&order, &refund); err != nil {
		h.t.Fatal(err)
	}
	// The owner's own user id: removing it is refused anyway, so a walk that
	// got through would still change nothing.
	var member string
	if err := testdb.Owner(h.t, h.pool).QueryRow(context.Background(), `select m.user_id::text from memberships m
			join organizers o on o.id = m.organizer_id where o.slug = $1 and m.role = 'owner'`, slug).Scan(&member); err != nil {
		h.t.Fatal(err)
	}
	return map[string]string{"organizer": slug, "member": member, "apiKey": apiKey, "event": event, "ticketType": ticketType, "quota": quota, "promoCode": promoCode, "question": question, "challengeId": "0190a0a0-0000-7000-8000-000000000000",
		"eventSlug": "gala", "token": "no-such-checkout-token-0000000000000000000", "order": order, "refund": refund, "code": "TS-00000", "q": "ab", "kind": "orders.csv"}
}

// walk calls every secured operation with token and checks the status (and
// error code, if want gives one); a zero status skips the operation.
func walk(t *testing.T, h *harness, token string, fixture map[string]string, want func(id string) (int, string)) {
	defer func(old *testing.T) { h.t = old }(h.t)
	h.t = t
	seen := map[string]bool{}
	checked := 0
	eachOp(t, fixture, func(id, method, _, url string) {
		seen[id] = true
		status, code := want(id)
		if status == 0 {
			return
		}
		res := h.do(token, method, "/v1"+url, `{}`)
		if got := h.errorCode(res); res.StatusCode != status || (code != "" && got != code) {
			t.Errorf("%s %s (%s): got %d %q, want %d %q", method, url, id, res.StatusCode, got, status, code)
		}
		checked++
	})
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

// eachOp calls fn for every secured operation in the spec, with its path
// template and a URL whose path and required query parameters come from
// fixture.
func eachOp(t *testing.T, fixture map[string]string, fn func(id, method, path, url string)) {
	t.Helper()
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{([^}]*)\}`)
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				continue // public; TestPublicOperationsAreExactlyThese pins the set
			}
			url := param.ReplaceAllStringFunc(path, func(m string) string {
				v, ok := fixture[m[1:len(m)-1]]
				if !ok {
					t.Errorf("%s: no fixture for path parameter %s; seed another tenant's real one", opID(op), m)
				}
				return v
			})
			// Without its required query parameters the generated wrapper
			// answers 400 before the handler can say 404.
			fn(opID(op), method, path, url+requiredQuery(t, op, fixture))
		}
	}
}

// requiredQuery is "?name=value&..." for op's required query parameters,
// each value from fixture. The generated wrapper reads them, as it does path
// parameters, before any middleware runs.
func requiredQuery(t *testing.T, op *openapi3.Operation, fixture map[string]string) string {
	t.Helper()
	q, sep := "", "?"
	for _, p := range op.Parameters {
		if p.Value.In == "query" && p.Value.Required {
			v, ok := fixture[p.Value.Name]
			if !ok {
				t.Errorf("%s: no fixture for query parameter %s", opID(op), p.Value.Name)
			}
			q += sep + p.Value.Name + "=" + v
			sep = "&"
		}
	}
	return q
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

// Spec §13.2 item 4: an owner of a suspended organizer can't start another
// one, and still reads the one they have.
func TestSuspendedMemberCantCreateOrganizer(t *testing.T) {
	h := newHarness(t)
	tok := h.signupWithMFA("owner@example.com")
	h.createOrganizer(tok, "acme")
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `with
		o as (select id from organizers where slug = 'acme'),
		u as (insert into users(email, name, email_verified_at) values ('staff@example.com', 'Staff', now()) returning id),
		st as (insert into staff(user_id, granted_by) select id, 'test' from u returning user_id),
		ss as (insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, expires_at)
			select st.user_id, gen_random_uuid(), o.id, 'acme', 'Acme', 'Checking a takedown report', now() + interval '1 hour' from st, o returning id, user_id, organizer_id),
		a as (insert into staff_actions(staff_session_id, user_id, organizer_id, kind, reason)
			select id, user_id, organizer_id, 'suspend', 'Checking a takedown report' from ss returning id, organizer_id)
		insert into organizer_suspensions(organizer_id, staff_action_id) select organizer_id, id from a`); err != nil {
		t.Fatal(err)
	}
	if res := h.do(tok, "POST", "/v1/organizers", `{"name":"Again","slug":"acme-two"}`); res.StatusCode != 403 || h.errorCode(res) != "suspended_member" {
		t.Fatalf("create while suspended: %d %q, want 403 suspended_member", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(tok, "GET", "/v1/organizers/acme", ""), 200)
	h.must(h.do(h.signupVerified("other@example.com"), "POST", "/v1/organizers", `{"name":"Other","slug":"other"}`), 201)
}
