package sales_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// offlineOrder seeds a paid comp or manual order of n General tickets, with
// no payment row and no fee, as Task 5's creation will leave it. The buyer
// is source@example.com.
func (f fixture) offlineOrder(t *testing.T, source, method string, total int64, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	code := make([]byte, 4)
	_, _ = rand.Read(code)
	var m *string
	if method != "" {
		m = &method
	}
	order := f.id(t, `insert into orders (organizer_id, event_id, code, email, subtotal_cents, total_cents, discount_cents, tax_cents, fee_cents, quote, details, status, paid_at, source, manual_method, fee_mode)
		values ($1, $2, $3, $4, $5, $5, 0, 0, 0, '{}', '{}', 'paid', now(), $6, $7, 'absorb') returning id`,
		f.org, f.event, "TS-"+strings.ToUpper(hex.EncodeToString(code))[:5], source+"@example.com", total, source, m)
	var ids []uuid.UUID
	for i := 0; i < n; i++ {
		secret := make([]byte, 16)
		_, _ = rand.Read(secret)
		ids = append(ids, f.id(t, `insert into tickets (organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, secret)
			values ($1, $2, $3, $4, $5, 0, 0, 0, $5, $6) returning id`, f.org, f.event, order, f.general, total/int64(n), secret))
	}
	return order, ids
}

// sentTo lists the send_email jobs queued for to.
func (f fixture) sentTo(t *testing.T, to string) (subjects, texts []string) {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), `select payload->>'subject', payload->>'text' from jobs where kind = 'send_email' and payload->>'to' = $1 order by id`, to)
	f.must(t, err)
	defer rows.Close()
	for rows.Next() {
		var s, x string
		f.must(t, rows.Scan(&s, &x))
		subjects, texts = append(subjects, s), append(texts, x)
	}
	f.must(t, rows.Err())
	return subjects, texts
}

func (f fixture) cancelEvent(t *testing.T, s *sales.Service) sales.CancelResult {
	t.Helper()
	var res sales.CancelResult
	f.must(t, db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		res, err = sales.CancelEvent(context.Background(), tx, f.org, f.event, "Gala", audit.Actor{User: f.user})
		return
	}))
	return res
}

func TestCancelEventCancelsManualAndComp(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	f.paidOrder(t, s, fake, 1, 1)
	comp, compTix := f.offlineOrder(t, "comp", "", 0, 2)
	manual, manualTix := f.offlineOrder(t, "manual", "cash", 4000, 1)
	var code string
	f.must(t, f.owner.QueryRow(context.Background(), `select code from orders where id = $1`, manual).Scan(&code))

	res := f.cancelEvent(t, s)
	want := []sales.ManualOrderTotal{{ID: manual, Code: code, TotalCents: 4000}}
	if len(res.ManualOrders) != 1 || res.ManualOrders[0] != want[0] || len(res.Failed) != 0 || res.Orders != 1 {
		t.Fatalf("result %+v", res)
	}
	if n := f.count(t, `select count(*) from refunds where reason = 'event_cancelled' and status = 'requested'`); n != 1 {
		t.Fatalf("%d requested refunds, want the online one", n)
	}
	// The online ticket is held until its refund runs.
	if f.quotaUsed(t) != 1 {
		t.Fatalf("quota used %d, want 1 (the online ticket, refund requested)", f.quotaUsed(t))
	}
	for _, c := range []struct {
		order uuid.UUID
		tix   []uuid.UUID
	}{{comp, compTix}, {manual, manualTix}} {
		if n := f.count(t, `select count(*) from orders where id = $1 and status = 'cancelled'`, c.order); n != 1 {
			t.Errorf("order %s isn't cancelled", c.order)
		}
		if n := f.count(t, `select count(*) from tickets where id = any($1) and status = 'void' and released_at is not null and refund_id is null`, c.tix); n != len(c.tix) {
			t.Errorf("%d of %d tickets void and released", n, len(c.tix))
		}
	}
	if n := f.count(t, `select count(*) from refunds r join payments p on p.id = r.payment_id where p.order_id = any($1)`, []uuid.UUID{comp, manual}); n != 0 {
		t.Fatalf("%d refunds on offline orders", n)
	}
	if subj, _ := f.sentTo(t, "comp@example.com"); len(subj) != 0 {
		t.Fatalf("comp buyer emailed: %v", subj)
	}
	subj, text := f.sentTo(t, "manual@example.com")
	if len(subj) != 1 || subj[0] != "Gala has been cancelled" ||
		text[0] != "Gala has been cancelled, so your tickets won't get you in. Acme will be in touch about your $40.00." {
		t.Fatalf("manual email %q %q", subj, text)
	}
	drain(t, s, &mailer.Memory{})
	if f.quotaUsed(t) != 0 {
		t.Fatalf("quota used %d after the refund ran, want 0", f.quotaUsed(t))
	}
}

func TestStaffRefundCancelsManualAndComp(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	_, compTix := f.offlineOrder(t, "comp", "", 0, 1)
	f.offlineOrder(t, "manual", "invoice", 4000, 1)
	f.suspend(t, f.org)
	res, err := s.RefundEventForStaff(context.Background(), f.org, f.refundAction(t, f.org), alertTo)
	f.must(t, err)
	if len(res.ManualOrders) != 1 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if n := f.count(t, `select count(*) from tickets where id = any($1) and status = 'void'`, compTix); n != 1 {
		t.Fatal("the comp ticket isn't void")
	}
	subj, text := f.sentTo(t, "manual@example.com")
	if len(subj) != 1 || !strings.HasPrefix(text[0], "Gala isn't being sold through TicketSlinger any more") {
		t.Fatalf("manual email %q %q", subj, text)
	}
}

func TestRefundRefusesOfflineOrders(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	for _, c := range []struct {
		source, method string
		total          int64
	}{{"comp", "", 0}, {"manual", "cash", 4000}} {
		order, _ := f.offlineOrder(t, c.source, c.method, c.total, 1)
		err := db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
			_, err := sales.RefundTickets(context.Background(), tx, f.org, f.event, order, sales.RefundRequest{}, "organizer", audit.Actor{User: f.user})
			return err
		})
		if conflictCode(err) != "no_payment" {
			t.Fatalf("%s: %v, want no_payment", c.source, err)
		}
	}
	if n := f.count(t, `select count(*) from tickets where status = 'valid'`); n != 2 {
		t.Fatalf("%d valid tickets, want 2", n)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 0 {
		t.Fatal("a refund was written")
	}
}

func TestManualCancelEmailFreeTotal(t *testing.T) {
	_, text := sales.ManualCancelEmail("event_cancelled", "Gala", "Acme", 0)
	if text != "Gala has been cancelled, so your tickets won't get you in." {
		t.Fatalf("%q", text)
	}
}
