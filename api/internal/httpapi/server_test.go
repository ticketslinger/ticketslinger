package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	res, err := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"email":"a@b.co","password":"x"}`))
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
