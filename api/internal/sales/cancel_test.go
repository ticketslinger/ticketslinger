package sales_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/checkin"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

func (f fixture) cancelTry(s *sales.Service, event, order uuid.UUID, in sales.CancelTicketsRequest) (res sales.CancelTicketsResult, err error) {
	err = db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		res, err = sales.CancelTickets(context.Background(), tx, f.org, event, order, in, audit.Actor{User: f.user})
		return
	})
	return res, err
}

func (f fixture) cancelTix(t *testing.T, s *sales.Service, order uuid.UUID, in sales.CancelTicketsRequest) sales.CancelTicketsResult {
	t.Helper()
	res, err := f.cancelTry(s, f.event, order, in)
	f.must(t, err)
	return res
}

func TestCancelVoidsAndFreesTheSeat(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, ids := f.paidOrder(t, s, fake, 3, 1)
	res := f.cancelTix(t, s, order, sales.CancelTicketsRequest{TicketIDs: ids[:2], Notify: true, Reason: "Double booked"})
	if !slices.Equal(res.TicketIDs, ids[:2]) || res.ChargeCents != 5000 || res.OrderStatus != "paid" || f.quotaUsed(t) != 1 {
		t.Fatalf("cancel %+v, quota used %d", res, f.quotaUsed(t))
	}
	if n := f.count(t, `select count(*) from tickets where id = any($1) and status = 'void' and released_at is not null and refund_id is null`, ids[:2]); n != 2 {
		t.Fatalf("%d tickets void and released, want 2", n)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 0 {
		t.Fatalf("%d refunds", n)
	}
	if n := f.count(t, `select count(*) from payments where order_id = $1 and status = 'succeeded'`, order); n != 1 {
		t.Fatal("the payment moved")
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	var cancelled []mailer.Message
	for _, e := range m.Sent {
		if strings.HasPrefix(e.Subject, "Tickets cancelled") {
			cancelled = append(cancelled, e)
		}
	}
	if len(cancelled) != 1 || cancelled[0].Subject != "Tickets cancelled for Gala" ||
		!strings.Contains(cancelled[0].Text, "Their reason: Double booked") || !strings.Contains(cancelled[0].Text, "Nothing has been refunded.") {
		t.Fatalf("emails %+v", m.Sent)
	}
	var after []byte
	var actor uuid.UUID
	f.must(t, f.owner.QueryRow(context.Background(), `select after, actor_user_id from audit_log where action = 'order.cancelled' and entity_id = $1`, order).Scan(&after, &actor))
	var got struct {
		TicketIDs []uuid.UUID `json:"ticket_ids"`
		Notify    bool        `json:"notify"`
		Reason    string      `json:"reason"`
		Via       string      `json:"via"`
	}
	f.must(t, json.Unmarshal(after, &got))
	if !slices.Equal(got.TicketIDs, ids[:2]) || !got.Notify || got.Reason != "Double booked" || got.Via != "organizer" || actor != f.user {
		t.Fatalf("audit %s by %s", after, actor)
	}
	res = f.cancelTix(t, s, order, sales.CancelTicketsRequest{})
	if res.OrderStatus != "cancelled" || !slices.Equal(res.TicketIDs, ids[2:]) {
		t.Fatalf("the rest %+v", res)
	}
	drain(t, s, m)
	n := 0
	for _, e := range m.Sent {
		if strings.HasPrefix(e.Subject, "Tickets cancelled") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d cancellation emails, want 1", n)
	}
}

func (f fixture) qrOf(t *testing.T, ticket uuid.UUID) string {
	t.Helper()
	var secret []byte
	f.must(t, f.owner.QueryRow(context.Background(), `select secret from tickets where id = $1`, ticket).Scan(&secret))
	return base64.RawURLEncoding.EncodeToString(secret)
}

func (f fixture) scan(t *testing.T, s *sales.Service, qr string) (r checkin.Result) {
	t.Helper()
	f.must(t, db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		r, err = checkin.CheckIn(context.Background(), tx, f.org, f.event, checkin.Input{QR: qr, Actor: audit.Actor{User: f.user}})
		return
	}))
	return r
}

func TestCancelledTicketDoesntScan(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, ids := f.paidOrder(t, s, fake, 1, 1)
	f.cancelTix(t, s, order, sales.CancelTicketsRequest{})
	if r := f.scan(t, s, f.qrOf(t, ids[0])); r.Result != "void" {
		t.Fatalf("scan after cancel: %q, want void", r.Result)
	}
}

func TestCancelCheckedInTicket(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, ids := f.paidOrder(t, s, fake, 1, 1)
	if r := f.scan(t, s, f.qrOf(t, ids[0])); r.Result != "accepted" {
		t.Fatalf("check-in: %q", r.Result)
	}
	f.cancelTix(t, s, order, sales.CancelTicketsRequest{})
	if n := f.count(t, `select count(*) from tickets where id = $1 and status = 'void' and released_at is not null`, ids[0]); n != 1 || f.quotaUsed(t) != 0 {
		t.Fatalf("void and released: %d, quota used %d", n, f.quotaUsed(t))
	}
}

func TestCancelRefusals(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 9, price: 2500})
	order, _, ids := f.paidOrder(t, s, fake, 2, 1)
	_, _, other := f.paidOrder(t, s, fake, 1, 2)
	otherEvent := f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'other', 'Other', 'America/New_York', now() + interval '6 days', now() + interval '7 days', 'live') returning id`, f.org)
	for name, c := range map[string]struct {
		in   sales.CancelTicketsRequest
		code string
	}{
		"empty list":             {sales.CancelTicketsRequest{TicketIDs: []uuid.UUID{}}, "bad_tickets"},
		"another order's":        {sales.CancelTicketsRequest{TicketIDs: other}, "bad_tickets"},
		"one of ours and theirs": {sales.CancelTicketsRequest{TicketIDs: []uuid.UUID{ids[0], other[0]}}, "bad_tickets"},
		"long reason":            {sales.CancelTicketsRequest{Reason: strings.Repeat("a", 501)}, "bad_reason"},
		"control in reason":      {sales.CancelTicketsRequest{Reason: "a\x00b"}, "bad_reason"},
		"too many":               {sales.CancelTicketsRequest{TicketIDs: make([]uuid.UUID, 101)}, "too_many"},
	} {
		if _, err := f.cancelTry(s, f.event, order, c.in); invalidCode(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := f.count(t, `select count(*) from tickets where status = 'void'`); n != 0 {
		t.Fatal("a refused cancel voided a ticket")
	}
	f.cancelTix(t, s, order, sales.CancelTicketsRequest{})
	if _, err := f.cancelTry(s, f.event, order, sales.CancelTicketsRequest{}); conflictCode(err) != "not_cancellable" {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := f.cancelTry(s, otherEvent, order, sales.CancelTicketsRequest{}); err == nil || invalidCode(err) != "" || conflictCode(err) != "" {
		t.Fatalf("other event: %v, want not found", err)
	}
}

// A refund in flight on A, then B cancelled: the order reads refunded, and A's
// seat comes back when the refund goes through.
func TestCancelBesideRefundInFlight(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, ids := f.paidOrder(t, s, fake, 2, 1)
	r := f.refund(t, s, order, sales.RefundRequest{TicketIDs: ids[:1]})
	res := f.cancelTix(t, s, order, sales.CancelTicketsRequest{})
	if res.OrderStatus != "refunded" || !slices.Equal(res.TicketIDs, ids[1:]) {
		t.Fatalf("cancel beside a refund %+v", res)
	}
	if f.quotaUsed(t) != 1 {
		t.Fatalf("quota used %d, want 1 (A is still held)", f.quotaUsed(t))
	}
	f.must(t, system.Run(context.Background(), s.Pool, f.org, func(tx pgx.Tx) error {
		return sales.FinishRefundForTest(s, context.Background(), tx, f.org, *r.RefundID, "succeeded")
	}))
	if n := f.count(t, `select count(*) from tickets where released_at is not null`); n != 2 || f.quotaUsed(t) != 0 {
		t.Fatalf("released %d, quota used %d", n, f.quotaUsed(t))
	}
	if st := f.str(t, `select status from orders where id = $1`, order); st != "refunded" {
		t.Fatalf("order %q, want refunded", st)
	}
}

func TestCancelEmailCopy(t *testing.T) {
	subject, text := sales.CancelEmailForTest("Gala", "Acme", "", []string{"- General, Ada"}, "online", "", 2600)
	want := "Acme has cancelled these tickets for Gala, so they won't get anyone in any more:\n\n- General, Ada\n\nNothing has been refunded. Questions? Contact the organizer, Acme."
	if subject != "Tickets cancelled for Gala" || text != want {
		t.Fatalf("%q\n%q", subject, text)
	}
	_, text = sales.CancelEmailForTest("Gala", "Acme", "Sorry", []string{"- General"}, "comp", "", 0)
	if strings.Contains(text, "Nothing has been refunded.") || !strings.Contains(text, "Their reason: Sorry\n\nQuestions?") {
		t.Fatalf("comp %q", text)
	}
	// An invoice wasn't paid, so there's nothing that wasn't refunded.
	_, text = sales.CancelEmailForTest("Gala", "Acme", "", []string{"- General"}, "manual", "invoice", 4000)
	if !strings.HasSuffix(text, "- General\n\nAcme will be in touch about the $40.00 invoice. Questions? Contact the organizer, Acme.") {
		t.Fatalf("invoice %q", text)
	}
	_, text = sales.CancelEmailForTest("Gala", "Acme", "", []string{"- General"}, "manual", "cash", 4000)
	if !strings.Contains(text, "Nothing has been refunded.") {
		t.Fatalf("cash %q", text)
	}
}

func TestCleanText(t *testing.T) {
	if got, ok := sales.CleanTextForTest(" a\r\nb ", 500); !ok || got != "a\nb" {
		t.Fatalf("%q %v", got, ok)
	}
	if _, ok := sales.CleanTextForTest(strings.Repeat("é", 500), 500); !ok {
		t.Error("500 runes refused")
	}
	if _, ok := sales.CleanTextForTest(strings.Repeat("é", 501), 500); ok {
		t.Error("501 runes allowed")
	}
	for _, bad := range []string{"\t", "\x00", "A\u202eB", "A\u2066B", "\xff"} {
		if _, ok := sales.CleanTextForTest(bad, 500); ok {
			t.Errorf("%q allowed", bad)
		}
	}
}
