package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
)

// unspecified are paths the API serves on purpose outside openapi.yaml.
var unspecified = []string{"/v1/webhooks/stripe", "/__test/"}

// clients holds the validating client for each test server, by base URL.
var clients sync.Map

type validating struct {
	t      *testing.T
	router routers.Router
	next   http.RoundTripper
}

// validatingClient checks every response against openapi.yaml, and every
// request that succeeded. A test that sends something the spec forbids on
// purpose gets a 4xx, so only its response is checked.
func validatingClient(t *testing.T, baseURL string) *http.Client {
	t.Helper()
	doc, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = openapi3.Servers{{URL: baseURL + "/v1"}}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &validating{t, router, http.DefaultTransport}}
}

// startServer serves h on a test server that do and doFrom can reach through
// the validating client. Close is registered with the test.
func startServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	clients.Store(srv.URL, validatingClient(t, srv.URL))
	return srv
}

// clientFor returns the validating client registered for rawURL's server.
func clientFor(t *testing.T, rawURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := clients.Load(u.Scheme + "://" + u.Host)
	if !ok {
		t.Fatalf("no validating client for %s; build the server with newAuthServerPool", rawURL)
	}
	return c.(*http.Client)
}

// bearerPresent is a real check, not a pass-through: a secured operation is
// satisfied only by a Bearer token. Whether the token is good is the server's
// business, and the tests assert on its answer.
func bearerPresent(_ context.Context, in *openapi3filter.AuthenticationInput) error {
	if !strings.HasPrefix(in.RequestValidationInput.Request.Header.Get("Authorization"), "Bearer ") {
		return errors.New("no bearer token")
	}
	return nil
}

func (v *validating) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	res, err := v.next.RoundTrip(req)
	if err != nil {
		return res, err
	}
	for _, p := range unspecified {
		if strings.HasPrefix(req.URL.Path, p) {
			return res, nil
		}
	}
	route, params, err := v.router.FindRoute(req)
	if err != nil {
		// An unknown path or method is only wrong if the server answered it.
		v.t.Errorf("%s %s isn't in openapi.yaml: %v", req.Method, req.URL.Path, err)
		return res, nil
	}
	resBody, _ := io.ReadAll(res.Body)
	res.Body = io.NopCloser(bytes.NewReader(resBody))
	opts := &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true, AuthenticationFunc: bearerPresent}
	in := &openapi3filter.RequestValidationInput{Request: req.Clone(context.Background()), PathParams: params, Route: route, Options: opts}
	in.Request.Body = io.NopCloser(bytes.NewReader(reqBody))
	if res.StatusCode < 300 {
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			v.t.Errorf("%s %s succeeded with a request the spec forbids: %v", req.Method, req.URL.Path, err)
		}
	}
	out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: res.StatusCode, Header: res.Header,
		Body: io.NopCloser(bytes.NewReader(resBody)), Options: opts}
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		v.t.Errorf("%s %s answered %d, which the spec doesn't describe: %v", req.Method, req.URL.Path, res.StatusCode, err)
	}
	return res, nil
}

// flatten gathers a schema's properties and required names, allOf included.
// allOf is an intersection, so a property seen in two parts keeps both, and
// every bound in either part applies.
func flatten(s *openapi3.Schema) (map[string][]*openapi3.Schema, map[string]bool) {
	props, req := map[string][]*openapi3.Schema{}, map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	for n, p := range s.Properties {
		props[n] = append(props[n], p.Value)
	}
	for _, part := range s.AllOf {
		p, r := flatten(part.Value)
		for n, v := range p {
			props[n] = append(props[n], v...)
		}
		for n := range r {
			req[n] = true
		}
	}
	return props, req
}

// sample is a value that passes the property's own bounds. Patterns can't be
// generated, so the few that the spec uses are named here; a new one fails
// the walk until it is added.
func sample(t *testing.T, name string, parts []*openapi3.Schema) any {
	t.Helper()
	s := parts[0]
	for _, p := range parts {
		if p.Type != nil || len(p.Enum) > 0 {
			s = p // the part that says what it is
		}
	}
	switch name {
	case "slug":
		return "fresh-slug"
	case "code":
		return "123456"
	case "timezone":
		return "Europe/London"
	case "starts_at":
		return "2027-03-01T19:00:00Z"
	case "ends_at":
		return "2027-03-01T22:00:00Z"
	}
	switch {
	case len(s.Enum) > 0:
		return s.Enum[0]
	case s.Type.Includes("string"):
		if s.Format == "email" {
			return "walk@example.com"
		}
		if s.Pattern != "" {
			t.Fatalf("no sample for %s (pattern %s); add one to sample()", name, s.Pattern)
		}
		return strings.Repeat("a", max(int(s.MinLength), 12))
	case s.Type.Includes("integer"):
		return 1
	case s.Type.Includes("array"):
		return []any{}
	case s.Type.Includes("boolean"):
		return true
	}
	t.Fatalf("no sample for %s (type %v); add one to sample()", name, s.Type)
	return nil
}

// outside returns values just past each bound the schema states.
func outside(parts []*openapi3.Schema) []any {
	var out []any
	for _, s := range parts {
		out = append(out, past(s)...)
	}
	return out
}

func past(s *openapi3.Schema) (out []any) {
	if s.MaxLength != nil {
		out = append(out, strings.Repeat("a", int(*s.MaxLength)+1))
	}
	if s.MinLength > 0 {
		out = append(out, "")
	}
	if s.Max != nil {
		out = append(out, *s.Max+1)
	}
	if s.Min != nil {
		out = append(out, *s.Min-1)
	}
	return out
}

// Every bound the spec states has to be enforced by Go: there is no runtime
// validation at the edge. One request per bound, just outside it, with every
// other required field valid so a missing one can't make the 422 for us.
func TestEveryBoundAnswers422(t *testing.T) {
	h := newHarness(t)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	acmeKey, _ := h.createKey(owner, "acme", "write")
	f := seedCatalogue(h, owner, "acme", acmeKey)
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{([^}]*)\}`)
	checked := 0
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody == nil || op.RequestBody.Value.Content.Get("application/json") == nil {
				continue
			}
			props, req := flatten(op.RequestBody.Value.Content.Get("application/json").Schema.Value)
			token := owner
			if op.Security != nil && len(*op.Security) == 0 {
				token = ""
			}
			target := param.ReplaceAllStringFunc(path, func(m string) string {
				v, ok := f[m[1:len(m)-1]]
				if !ok {
					t.Errorf("%s: no fixture for %s", opID(op), m)
				}
				return v
			})
			for name, parts := range props {
				for _, bad := range outside(parts) {
					body := map[string]any{}
					for n := range req {
						body[n] = sample(t, n, props[n])
					}
					body[name] = bad
					b, _ := json.Marshal(body)
					res := h.do(token, method, "/v1"+target, string(b))
					code := h.errorCode(res)
					if res.StatusCode != http.StatusUnprocessableEntity || code == "missing_field" {
						t.Errorf("%s /v1%s with %s=%.20v: got %d %q, want 422 for the bound", method, target, name, bad, res.StatusCode, code)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no bounds checked; the walk is broken")
	}
	t.Logf("%d bounds checked", checked)
}
