package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func (s shop) checkinPath(rest string) string {
	return "/v1/organizers/acme/events/" + s.f["event"] + "/checkins" + rest
}

// doorTickets is a paid order of two tickets (Ada's first) and their QR texts.
func (s shop) doorTickets() (code string, qrs []string) {
	s.t.Helper()
	order, code, token := s.buyLinked(2, "buyer@example.com", "evt_door_paid")
	// Ada's is the first by id, the order the page lists them in.
	if _, err := testdb.Owner(s.t, s.pool).Exec(context.Background(), `update tickets set attendee_name = 'Ada'
		where id = (select min(id::text)::uuid from tickets where order_id = $1)`, order); err != nil {
		s.t.Fatal(err)
	}
	o := s.must(s.send("", "", "GET", "/v1/orders/"+code+"?token="+url.QueryEscape(token), ""), 200)
	for _, x := range o["tickets"].([]any) {
		qrs = append(qrs, x.(map[string]any)["qr"].(string))
	}
	return code, qrs
}

func TestCheckinRolesAndKeys(t *testing.T) {
	s := newShop(t, nil)
	_, qrs := s.doorTickets()
	body := `{"qr":"` + qrs[0] + `","gate":"North"}`
	_, writeKey := s.createKey(s.owner, "acme", "write")
	admin := s.member("admin@example.com", "admin")
	door := s.member("door@example.com", "checkin")
	for i, who := range []string{s.owner, admin, door, writeKey} {
		r := s.must(s.do(who, "POST", s.checkinPath(""), body), 200)
		want := "duplicate"
		if i == 0 {
			want = "accepted"
		}
		if r["result"] != want || r["checked_in"] != float64(1) || r["sold"] != float64(2) {
			t.Fatalf("caller %d: %v, want %s", i, r, want)
		}
	}
	_, readKey := s.createKey(s.owner, "acme", "read")
	if res := s.do(readKey, "POST", s.checkinPath(""), body); res.StatusCode != 403 || s.errorCode(res) != "scope_read_only" {
		t.Fatalf("read key: %d %q, want 403 scope_read_only", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(s.owner, "POST", "/v1/organizers/acme/events/"+uuid.NewString()+"/checkins", body); res.StatusCode != 404 {
		t.Fatalf("unknown event: %d, want 404", res.StatusCode)
	}

	// Door staff don't see money (spec D22): the orders list and one order
	// answer forbidden_role without a cent in the body; editors read both.
	var order string
	if err := testdb.Owner(t, s.pool).QueryRow(context.Background(), `select id::text from orders where email = 'buyer@example.com'`).Scan(&order); err != nil {
		t.Fatal(err)
	}
	list := "/v1/organizers/acme/events/" + s.f["event"] + "/orders"
	for _, path := range []string{list, s.orderPath(order)} {
		res := s.do(door, "GET", path, "")
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 403 || !strings.Contains(string(b), `"forbidden_role"`) {
			t.Fatalf("door %s: %d %s, want 403 forbidden_role", path, res.StatusCode, b)
		}
		for _, money := range []string{"total_cents", "fee_cents", "refunded_cents", "charge_cents"} {
			if strings.Contains(string(b), money) {
				t.Fatalf("door %s: %s carries %s", path, b, money)
			}
		}
		for _, who := range []string{s.owner, admin} {
			if res := s.do(who, "GET", path, ""); res.StatusCode != 200 {
				t.Fatalf("editor %s: %d, want 200", path, res.StatusCode)
			}
		}
	}
}

func TestCheckinNeedsExactlyOne(t *testing.T) {
	s := newShop(t, nil)
	for _, body := range []string{`{}`, `{"gate":"North"}`, `{"qr":"AAAAAAAAAAAAAAAAAAAAAA","ticket_id":"` + uuid.NewString() + `"}`} {
		if res := s.do(s.owner, "POST", s.checkinPath(""), body); res.StatusCode != 422 || s.errorCode(res) != "qr_or_ticket" {
			t.Errorf("%s: %d %q, want 422 qr_or_ticket", body, res.StatusCode, s.errorCode(res))
		}
	}
	r := s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"ticket_id":"`+uuid.NewString()+`"}`), 200)
	if r["result"] != "invalid" || r["ticket"] != nil {
		t.Fatalf("unknown ticket id: %v, want invalid with a null ticket", r)
	}
}

func TestCheckinRetryReplaysTheFirstAnswer(t *testing.T) {
	s := newShop(t, nil)
	_, qrs := s.doorTickets()
	body := `{"qr":"` + qrs[0] + `"}`
	key := uuid.NewString()
	for i, want := range []string{"", "true"} {
		res, b := s.doKeyed(s.owner, "POST", s.checkinPath(""), key, body)
		if res.StatusCode != 200 || !strings.Contains(b, `"result":"accepted"`) || res.Header.Get("Idempotent-Replayed") != want {
			t.Fatalf("try %d: %d %s replayed %q", i, res.StatusCode, b, res.Header.Get("Idempotent-Replayed"))
		}
	}
	if _, b := s.doKeyed(s.owner, "POST", s.checkinPath(""), uuid.NewString(), body); !strings.Contains(b, `"result":"duplicate"`) {
		t.Fatalf("a new key: %s, want duplicate", b)
	}
}

func TestCheckinSearchAndSummary(t *testing.T) {
	s := newShop(t, nil)
	code, qrs := s.doorTickets()
	door := s.member("door@example.com", "checkin")
	r := s.must(s.do(door, "POST", s.checkinPath(""), `{"qr":"`+qrs[0]+`"}`), 200)
	tk, _ := r["ticket"].(map[string]any)
	if r["result"] != "accepted" || tk["attendee_name"] != "Ada" || tk["order_code"] != code || tk["checked_in_at"] == nil {
		t.Fatalf("scan: %v", r)
	}
	found := s.jsonList(s.do(door, "GET", s.checkinPath("/search?q=ada"), ""))
	if len(found) != 1 {
		t.Fatalf("search ada: %v", found)
	}
	if f := found[0].(map[string]any); f["id"] != tk["id"] || f["order_code"] != code || f["status"] != "valid" || f["checked_in_at"] == nil {
		t.Fatalf("search ada: %v", f)
	}
	// The buyer's email finds their tickets but never comes back: the door
	// sees a name and an order code, not an address to copy down.
	byEmail := s.jsonList(s.do(door, "GET", s.checkinPath("/search?q=buyer@example"), ""))
	if len(byEmail) != 2 {
		t.Fatalf("search by email: %v", byEmail)
	}
	for _, got := range append(byEmail, tk) {
		if _, has := got.(map[string]any)["email"]; has {
			t.Fatalf("a check-in ticket carries the buyer's email: %v", got)
		}
	}
	sum := s.must(s.do(door, "GET", s.checkinPath("/summary"), ""), 200)
	if sum["checked_in"] != float64(1) || sum["sold"] != float64(2) {
		t.Fatalf("summary: %v", sum)
	}
}

func TestCheckinLogsNoQR(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := newShop(t, nil)
	_, qrs := s.doorTickets()
	s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+qrs[0]+`"}`), 200)
	for _, body := range []string{`{"qr":"` + qrs[1] + `","ticket_id":"` + uuid.NewString() + `"}`, `{"qr":"` + qrs[1] + `","gate":5}`} {
		if res := s.do(s.owner, "POST", s.checkinPath(""), body); res.StatusCode < 400 {
			t.Fatalf("%s: %d, want a 4xx", body, res.StatusCode)
		}
	}
	got := logs.String()
	if !strings.Contains(got, "bad request") {
		t.Fatalf("expected the bad request in the log:\n%s", got)
	}
	for _, qr := range qrs {
		if strings.Contains(got, qr) {
			t.Fatalf("the log carries a scanned QR:\n%s", got)
		}
	}
}

func TestCheckinBounds(t *testing.T) {
	s := newShop(t, nil)
	field := func(method, path, body, want string) {
		t.Helper()
		res := s.do(s.owner, method, path, body)
		b := s.json(res)
		if res.StatusCode != 422 || errorField(b) != want {
			t.Errorf("%s %.60s %.60s: %d %v, want 422 field %s", method, path, body, res.StatusCode, b, want)
		}
	}
	field("POST", s.checkinPath(""), `{"qr":"`+strings.Repeat("a", 201)+`"}`, "qr")
	field("POST", s.checkinPath(""), `{"qr":""}`, "qr")
	field("POST", s.checkinPath(""), `{"qr":"AAAAAAAAAAAAAAAAAAAAAA","gate":"`+strings.Repeat("g", 41)+`"}`, "gate")
	field("GET", s.checkinPath("/search?q=a"), "", "q")
	field("GET", s.checkinPath("/search?q=%20a%20"), "", "q")
	field("GET", s.checkinPath("/search?q="+strings.Repeat("a", 121)), "", "q")
	field("GET", s.checkinPath("/search?q="+strings.Repeat("a", 120)+"%20"), "", "q")
	s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"`+strings.Repeat("é", 200)+`","gate":"`+strings.Repeat("é", 40)+`"}`), 200)
	if list := s.jsonList(s.do(s.owner, "GET", s.checkinPath("/search?q=ab"), "")); len(list) != 0 {
		t.Fatalf("search ab: %v", list)
	}
	s.must(s.do(s.owner, "GET", s.checkinPath("/search?q="+strings.Repeat("a", 120)), ""), 200)
}

// Search reads names and order codes, so one caller can't page through
// every ticket of an event without end.
func TestCheckinSearchIsRateLimited(t *testing.T) {
	s := newShop(t, nil)
	for i := range 600 {
		if res := s.do(s.owner, "GET", s.checkinPath("/search?q=ada"), ""); res.StatusCode != 200 {
			t.Fatalf("search %d: %d", i, res.StatusCode)
		}
	}
	res := s.do(s.owner, "GET", s.checkinPath("/search?q=ada"), "")
	if res.StatusCode != 429 || s.errorCode(res) != "rate_limited" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("search 601: %d %q, want 429 rate_limited with Retry-After", res.StatusCode, s.errorCode(res))
	}
	// Scans keep their own count, and another caller has a count of its own.
	s.must(s.do(s.owner, "POST", s.checkinPath(""), `{"qr":"not-a-ticket"}`), 200)
	_, key := s.createKey(s.owner, "acme", "read")
	s.must(s.do(key, "GET", s.checkinPath("/search?q=ada"), ""), 200)
}

// Every invalid scan writes a checkins row, so one caller can't scan without end.
func TestCheckinIsRateLimited(t *testing.T) {
	s := newShop(t, nil)
	body := `{"qr":"not-a-ticket"}`
	for i := range 300 {
		if res := s.do(s.owner, "POST", s.checkinPath(""), body); res.StatusCode != 200 {
			t.Fatalf("scan %d: %d", i, res.StatusCode)
		}
	}
	res := s.do(s.owner, "POST", s.checkinPath(""), body)
	if res.StatusCode != 429 || s.errorCode(res) != "rate_limited" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("scan 301: %d %q, want 429 rate_limited with Retry-After", res.StatusCode, s.errorCode(res))
	}
	// Another caller has a count of its own.
	_, key := s.createKey(s.owner, "acme", "write")
	s.must(s.do(key, "POST", s.checkinPath(""), body), 200)
	if n := s.count(`select count(*) from checkins`); n != 301 {
		t.Fatalf("checkins rows: %d, want 301", n)
	}
}
