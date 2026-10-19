package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
)

func TestHealth(t *testing.T) {
	srv := startServer(t, httpapi.New(httpapi.Deps{}))
	res, err := clientFor(t, srv.URL).Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 200 || body["status"] != "ok" {
		t.Fatalf("status=%d body=%v", res.StatusCode, body)
	}
}

func TestUnknownRouteIsJSONError(t *testing.T) {
	// Raw on purpose: /v1/nope isn't in the spec, so the validating client
	// would (rightly) call the request itself an error.
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{})) // raw: /v1/nope is outside the spec
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/nope") // raw: /v1/nope is outside the spec
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 404 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d type=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

// Every secured operation must answer 401 to an unauthenticated request with
// an empty body, so authentication can't slip behind body decoding.
func TestSecuredOperationsReject401BeforeBody(t *testing.T) {
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, httpapi.New(httpapi.Deps{}))
	n := 0
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			secured := op.Security == nil && len(spec.Security) > 0 || op.Security != nil && len(*op.Security) > 0
			if !secured {
				continue
			}
			n++
			// Every path parameter in the spec is a uuid.
			p := regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(path, "0197a2b4-0000-7000-8000-000000000000")
			req, _ := http.NewRequest(method, srv.URL+"/v1"+p, nil)
			res, err := clientFor(t, srv.URL).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != 401 {
				t.Errorf("%s %s (%s): status %d, want 401", method, path, op.OperationID, res.StatusCode)
			}
		}
	}
	if n == 0 {
		t.Fatal("no secured operations found")
	}
}

// The operations that need no session are exactly these. Adding
// `security: []` to anything else fails here, so it can't slip in unreviewed.
func TestPublicOperationsAreExactlyThese(t *testing.T) {
	want := []string{"beginPasskeyLogin", "createCheckout", "finishPasskeyLogin", "getCheckout", "getHealth", "getOrder", "getOrderPDF", "getStorefrontEvent",
		"login", "payCheckout", "requestOrderLinks", "requestTicketEmail", "signup", "verifyEmail"}
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range spec.Paths.Map() {
		for _, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				got = append(got, opID(op))
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("public operations = %v, want %v", got, want)
	}
}

// The storefront keeps these first URL segments for itself.
func TestCreateOrganizerRefusesReservedSlugs(t *testing.T) {
	h := newHarness(t)
	tok := h.signupVerified("owner@example.com")
	for _, slug := range []string{"checkout", "orders", "assets", "api", "healthz"} {
		res := h.do(tok, "POST", "/v1/organizers", `{"name":"Acme","slug":"`+slug+`"}`)
		e, _ := h.json(res)["error"].(map[string]any)
		if res.StatusCode != 422 || e["code"] != "invalid_slug" || e["field"] != "slug" ||
			e["message"] != "That web address is taken by the ticket site itself. Pick another." {
			t.Errorf("%s: got %d %v", slug, res.StatusCode, e)
		}
	}
	h.must(h.do(tok, "POST", "/v1/organizers", `{"name":"Acme","slug":"orders-co"}`), 201)
}
