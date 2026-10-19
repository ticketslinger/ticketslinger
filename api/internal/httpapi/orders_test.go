package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// buy checks out qty tickets for email and pays; with evt it also sends
// Stripe's payment_intent.succeeded as event evt.
func (s shop) buy(qty int, email, evt string) {
	s.t.Helper()
	token := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], qty, "")), 201)["token"].(string)
	pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"`+email+`"}`), 200)
	if evt == "" {
		return
	}
	pi, _, _ := strings.Cut(pay["client_secret"].(string), "_secret_")
	s.must(s.webhook(event(evt, "payment_intent.succeeded", map[string]any{"id": pi, "object": "payment_intent",
		"amount": pay["amount_cents"], "currency": "usd", "status": "succeeded"}), webhookSecret, time.Now()), 200)
}

// buyPaid is a paid order of qty tickets for email, settled by Stripe event evt.
func (s shop) buyPaid(qty int, email, evt string) (orderID string) {
	s.t.Helper()
	s.buy(qty, email, evt)
	if err := testdb.Owner(s.t, s.pool).QueryRow(context.Background(), `select id::text from orders where email = $1`, email).Scan(&orderID); err != nil {
		s.t.Fatal(err)
	}
	return orderID
}

func TestListOrders(t *testing.T) {
	s := newShop(t, nil)
	path := "/v1/organizers/acme/events/" + s.f["event"] + "/orders"
	// The fixture's own order (seed@example.com) is listed throughout.
	bought := func(who string) (out []map[string]any) {
		t.Helper()
		for _, o := range s.jsonList(s.do(who, "GET", path, "")) {
			if o := o.(map[string]any); o["email"] != "seed@example.com" {
				out = append(out, o)
			}
		}
		return out
	}
	if list := bought(s.owner); len(list) != 0 {
		t.Fatalf("before any sale: %v", list)
	}
	s.buy(1, "pending@example.com", "")
	s.buyPaid(2, "paid@example.com", "evt_orders_paid")

	_, key := s.createKey(s.owner, "acme", "read")
	for _, who := range []string{s.owner, key} {
		list := bought(who)
		if len(list) != 1 {
			t.Fatalf("listed %d orders, want only the paid one: %v", len(list), list)
		}
		o := list[0]
		if o["email"] != "paid@example.com" || o["status"] != "paid" || o["ticket_count"] != float64(2) || o["total_cents"] != float64(5200) ||
			o["fee_mode"] != "pass" || o["paid_at"] == nil || o["refunded_cents"] != float64(0) || o["fee_refunded_cents"] != float64(0) {
			t.Fatalf("order = %v", o)
		}
	}
	if res := s.do(s.owner, "GET", "/v1/organizers/acme/events/00000000-0000-0000-0000-000000000000/orders", ""); res.StatusCode != 404 {
		t.Fatalf("unknown event: %d", res.StatusCode)
	}
}

// jsonList reads a JSON array body.
func (h *harness) jsonList(res *http.Response) []any {
	h.t.Helper()
	b, _ := io.ReadAll(res.Body)
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("%d %s", res.StatusCode, b)
	}
	return out
}

func TestResendTicketEmailAPI(t *testing.T) {
	s := newShop(t, nil)
	order := s.buyPaid(1, "buyer@example.com", "evt_resend_paid")
	path := s.orderPath(order) + "/email"
	root := testdb.Owner(t, s.pool)
	sentAgo := func(minutes int) {
		t.Helper()
		if _, err := root.Exec(context.Background(), `update orders set mail_status = 'sent',
			mail_requested_at = now() - make_interval(mins => $2) where id = $1`, order, minutes); err != nil {
			t.Fatal(err)
		}
	}
	conflict := func(who, code string) {
		t.Helper()
		if res := s.do(who, "POST", path, ""); res.StatusCode != 409 || s.errorCode(res) != code {
			t.Fatalf("%d %q, want 409 %s", res.StatusCode, s.errorCode(res), code)
		}
	}
	conflict(s.owner, "email_on_its_way") // the first one is still queued

	_, writeKey := s.createKey(s.owner, "acme", "write")
	for _, who := range []string{s.owner, writeKey} {
		sentAgo(11)
		r := s.must(s.do(who, "POST", path, ""), 202)
		if r["status"] != "queued" || r["problem"] != nil || r["attempts"] != float64(0) || r["updated_at"] == nil {
			t.Fatalf("resent: %v", r)
		}
		conflict(who, "email_on_its_way")
	}
	sentAgo(5)
	conflict(s.owner, "too_soon")

	_, readKey := s.createKey(s.owner, "acme", "read")
	sentAgo(11)
	for _, who := range []string{readKey, s.member("door@example.com", "checkin")} {
		if res := s.do(who, "POST", path, ""); res.StatusCode != 403 {
			t.Fatalf("%d, want 403", res.StatusCode)
		}
	}
	if _, err := root.Exec(context.Background(), `update tickets set status = 'void' where order_id = $1`, order); err != nil {
		t.Fatal(err)
	}
	conflict(s.owner, "no_tickets")

	d := s.must(s.do(s.owner, "GET", s.orderPath(order), ""), 200)
	if e, _ := d["ticket_email"].(map[string]any); e["status"] != "sent" {
		t.Fatalf("order detail ticket_email %v, want sent", d["ticket_email"])
	}
}

// buyLinked is a paid order of qty tickets for email, settled by Stripe
// event evt, with the code and token from its link.
func (s shop) buyLinked(qty int, email, evt string) (order, code, token string) {
	s.t.Helper()
	checkout := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], qty, "")), 201)["token"].(string)
	pay := s.must(s.send("", "", "POST", "/v1/checkouts/"+checkout+"/pay", `{"email":"`+email+`"}`), 200)
	pi, _, _ := strings.Cut(pay["client_secret"].(string), "_secret_")
	s.must(s.webhook(event(evt, "payment_intent.succeeded", map[string]any{"id": pi, "object": "payment_intent",
		"amount": pay["amount_cents"], "currency": "usd", "status": "succeeded"}), webhookSecret, time.Now()), 200)
	c := s.must(s.send("", "", "GET", "/v1/checkouts/"+checkout, ""), 200)
	link, err := url.Parse(c["order_url"].(string))
	if err != nil {
		s.t.Fatal(err)
	}
	if err := testdb.Owner(s.t, s.pool).QueryRow(context.Background(), `select id::text from orders where email = $1`, email).Scan(&order); err != nil {
		s.t.Fatal(err)
	}
	return order, c["order_code"].(string), link.Query().Get("t")
}

// voidFirst voids the order's first ticket by id, the order the page, the
// email and the PDF number them in.
func (s shop) voidFirst(order string) {
	s.t.Helper()
	if _, err := testdb.Owner(s.t, s.pool).Exec(context.Background(), `update tickets set status = 'void'
		where id = (select min(id::text)::uuid from tickets where order_id = $1)`, order); err != nil {
		s.t.Fatal(err)
	}
}

func TestOrderPageCarriesQRImages(t *testing.T) {
	s := newShop(t, nil)
	order, code, token := s.buyLinked(2, "qr@example.com", "evt_qr_paid")
	s.voidFirst(order)
	o := s.must(s.send("", "", "GET", "/v1/orders/"+code+"?token="+url.QueryEscape(token), ""), 200)
	tickets, _ := o["tickets"].([]any)
	if len(tickets) != 2 {
		t.Fatalf("tickets = %v", tickets)
	}
	for _, x := range tickets {
		tk := x.(map[string]any)
		if tk["status"] == "void" {
			if tk["qr_png"] != nil {
				t.Fatalf("void ticket has a QR image: %v", tk)
			}
			continue
		}
		enc, _ := tk["qr_png"].(string)
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 264 || img.Bounds().Dy() != 264 {
			t.Fatalf("qr_png: %v %v, want a 264x264 PNG", err, img.Bounds())
		}
	}
}

func TestOrderPDFDownload(t *testing.T) {
	s := newShop(t, nil)
	order, code, token := s.buyLinked(3, "pdf@example.com", "evt_pdf_paid")
	s.voidFirst(order)
	get := func(code, token string) *http.Response {
		return s.send("", "", "GET", "/v1/orders/"+code+"/tickets.pdf?token="+url.QueryEscape(token), "")
	}
	res := get(code, token)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatalf("%d %.40q, want a PDF", res.StatusCode, b)
	}
	if h := res.Header; h.Get("Content-Type") != "application/pdf" || h.Get("Content-Disposition") != `attachment; filename="tickets-`+code+`.pdf"` ||
		h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers %v", h)
	}
	if n := len(regexp.MustCompile(`/Type\s*/Page\b`).FindAll(b, -1)); n != 2 {
		t.Fatalf("%d pages, want one per valid ticket (2)", n)
	}
	body := func(res *http.Response) string {
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	wrong, unknown := get(code, "wrong"), get("TS-00000", token)
	if wb, ub := body(wrong), body(unknown); wrong.StatusCode != 404 || unknown.StatusCode != 404 || wb != ub {
		t.Fatalf("wrong token %d %s; unknown code %d %s; want the same 404", wrong.StatusCode, wb, unknown.StatusCode, ub)
	}
	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update tickets set status = 'void' where order_id = $1`, order); err != nil {
		t.Fatal(err)
	}
	if res := get(code, token); res.StatusCode != 409 || s.errorCode(res) != "no_tickets" {
		t.Fatalf("all void: %d %q, want 409 no_tickets", res.StatusCode, s.errorCode(res))
	}
}

func TestBuyerCanAskForTheEmailAgain(t *testing.T) {
	s := newShop(t, nil)
	order, code, token := s.buyLinked(1, "again@example.com", "evt_again_paid")
	root := testdb.Owner(t, s.pool)
	ctx := context.Background()
	ask := func(ip, code, token string) *http.Response {
		b, _ := json.Marshal(map[string]string{"token": token})
		return s.send(ip, "", "POST", "/v1/orders/"+code+"/email", string(b))
	}
	conflict := func(want string) {
		t.Helper()
		if res := ask("", code, token); res.StatusCode != 409 || s.errorCode(res) != want {
			t.Fatalf("%d %q, want 409 %s", res.StatusCode, s.errorCode(res), want)
		}
	}
	jobs := func() (n int) {
		t.Helper()
		if err := root.QueryRow(ctx, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1`, order).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := root.Exec(ctx, `update orders set mail_status = 'sent', mail_requested_at = now() - interval '11 minutes' where id = $1`, order); err != nil {
		t.Fatal(err)
	}
	before := jobs()
	if res := ask("", code, token); res.StatusCode != 202 {
		t.Fatalf("%d %q, want 202", res.StatusCode, s.errorCode(res))
	}
	if jobs() != before+1 {
		t.Fatal("no job queued")
	}
	conflict("email_on_its_way")
	if _, err := root.Exec(ctx, `update orders set mail_status = 'sent' where id = $1`, order); err != nil {
		t.Fatal(err)
	}
	conflict("too_soon")

	body := func(res *http.Response) string {
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	wrong, unknown := ask("", code, "wrong"), ask("", "TS-00000", token)
	if wb, ub := body(wrong), body(unknown); wrong.StatusCode != 404 || unknown.StatusCode != 404 || wb != ub {
		t.Fatalf("wrong token %d %s; unknown code %d %s; want the same 404", wrong.StatusCode, wb, unknown.StatusCode, ub)
	}
	if _, err := root.Exec(ctx, `update orders set mail_requested_at = now() - interval '11 minutes' where id = $1`, order); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec(ctx, `update tickets set status = 'void' where order_id = $1`, order); err != nil {
		t.Fatal(err)
	}
	conflict("no_tickets")
	if jobs() != before+1 {
		t.Fatal("a refused request queued a job")
	}

	for range 60 {
		s.must(ask("203.0.113.9", code, "wrong"), 404)
	}
	if res := ask("203.0.113.9", code, token); res.StatusCode != 429 || s.errorCode(res) != "rate_limited" {
		t.Fatalf("61st request: %d %q, want 429", res.StatusCode, s.errorCode(res))
	}
}
