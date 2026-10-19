package httpapi_test

import (
	"io"
	"strings"
	"testing"
)

// Reports carry money, so they follow the orders' access: owners, admins and
// keys read them; door staff get forbidden_role.
func TestReportAccess(t *testing.T) {
	s := newShop(t, nil)
	s.buyPaid(2, "paid@example.com", "evt_report_paid") // seedCatalogue already sold one, with no tickets
	base := "/v1/organizers/acme/events/" + s.f["event"]
	_, readKey := s.createKey(s.owner, "acme", "read")
	door := s.member("door@example.com", "checkin")

	for _, who := range []string{s.owner, readKey} {
		r := s.must(s.do(who, "GET", base+"/report", ""), 200)
		if r["currency"] != "USD" || r["orders"] != float64(2) || r["sold"] != float64(2) || r["gross_cents"].(float64) <= 0 {
			t.Fatalf("report: %v", r)
		}
	}
	for _, path := range []string{base + "/report", base + "/exports/orders.csv", base + "/exports/attendees.csv", base + "/exports/checkins.csv"} {
		res := s.do(door, "GET", path, "")
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 403 || !strings.Contains(string(b), `"forbidden_role"`) {
			t.Fatalf("door %s: %d %s, want 403 forbidden_role", path, res.StatusCode, b)
		}
	}

	for _, who := range []string{s.owner, readKey} {
		res := s.do(who, "GET", base+"/exports/orders.csv", "")
		b, _ := io.ReadAll(res.Body)
		h := res.Header
		if res.StatusCode != 200 || h.Get("Content-Type") != "text/csv; charset=utf-8" ||
			h.Get("Content-Disposition") != `attachment; filename="gala-orders.csv"` || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("orders.csv: %d %v", res.StatusCode, h)
		}
		if !strings.HasPrefix(string(b), "\xEF\xBB\xBFOrder,Status,Email,") || !strings.Contains(string(b), "paid@example.com") {
			t.Fatalf("orders.csv body: %q", b)
		}
	}
	if res := s.do(s.owner, "GET", base+"/exports/secrets.csv", ""); res.StatusCode != 400 {
		t.Fatalf("unknown kind: %d, want 400", res.StatusCode)
	}
}
