package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
)

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/health")
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
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 404 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d type=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestUnbuiltOperationIs501JSON(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
	res, err := http.Post(srv.URL+"/v1/auth/passkeys/login", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Error struct{ Code, Message string }
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 501 || body.Error.Code != "not_implemented" {
		t.Fatalf("status=%d body=%+v", res.StatusCode, body)
	}
}

// Every secured operation must answer 401 to an unauthenticated request with
// an empty body, so authentication can't slip behind body decoding.
func TestSecuredOperationsReject401BeforeBody(t *testing.T) {
	spec, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
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
			res, err := http.DefaultClient.Do(req)
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
