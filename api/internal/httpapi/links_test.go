package httpapi_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// Asking for a link for an address that bought nothing looks exactly like
// asking for one that did: same status, headers and body.
func TestOrderLinkRequestsLookTheSame(t *testing.T) {
	s := newShop(t, nil)
	s.buyPaid(1, "buyer@example.com", "evt_links_1")
	answer := func(ip, email string) (int, http.Header, string) {
		res := s.send(ip, "", "POST", "/v1/storefront/acme/gala/order-links", `{"email":"`+email+`"}`)
		body, _ := io.ReadAll(res.Body)
		h := res.Header.Clone()
		h.Del("Date")
		h.Del("X-Request-Id")
		return res.StatusCode, h, string(body)
	}
	knownStatus, knownHeader, knownBody := answer("203.0.113.20", "buyer@example.com")
	strangerStatus, strangerHeader, strangerBody := answer("203.0.113.21", "nobody@example.com")
	if knownStatus != 204 || strangerStatus != 204 || knownBody != strangerBody || !maps.EqualFunc(knownHeader, strangerHeader, slices.Equal) {
		t.Fatalf("known %d %v %q; stranger %d %v %q", knownStatus, knownHeader, knownBody, strangerStatus, strangerHeader, strangerBody)
	}
	var n int
	if err := testdb.Owner(t, s.pool).QueryRow(context.Background(), `select count(*) from jobs where kind = 'order_links'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d order_links jobs, want 2 (%v)", n, err)
	}
	if res := s.send("203.0.113.22", "", "POST", "/v1/storefront/acme/gala/order-links", `{"email":"not-an-email"}`); res.StatusCode != 422 || s.errorCode(res) != "bad_email" {
		t.Fatalf("bad email: %d %q, want 422 bad_email", res.StatusCode, s.errorCode(res))
	}
}

func TestOrderLinkRequestsAreRateLimited(t *testing.T) {
	s := newShop(t, nil)
	for range 5 {
		s.must(s.send("203.0.113.30", "", "POST", "/v1/storefront/acme/gala/order-links", `{"email":"buyer@example.com"}`), 204)
	}
	if res := s.send("203.0.113.30", "", "POST", "/v1/storefront/acme/gala/order-links", `{"email":"buyer@example.com"}`); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("6th request in an hour: %d %q, want 429 rate_limited", res.StatusCode, s.errorCode(res))
	}
	s.must(s.send("203.0.113.31", "", "POST", "/v1/storefront/acme/gala/order-links", `{"email":"buyer@example.com"}`), 204)
}
