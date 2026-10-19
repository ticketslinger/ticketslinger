package sales_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// runKind runs a worker with just h for kind until nothing is due.
func runKind(t *testing.T, s *sales.Service, kind string, h jobs.Handler) {
	t.Helper()
	w := &jobs.Worker{Pool: s.Pool, Handlers: map[string]jobs.Handler{kind: h}}
	for {
		ran, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func (f fixture) str(t *testing.T, sql string, args ...any) (s string) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), sql, args...).Scan(&s))
	return s
}

func TestOrderLinkAgainReplacesTheOldOne(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	order, _, _ := f.paidOrder(t, s, fake, 1, 1) // buyer@example.com
	drain(t, s, &mailer.Memory{})
	code := f.str(t, `select code from orders where id = $1`, order)
	old := sales.OrderToken(s.SecretKey, order)
	f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "Buyer@Example.com"))
	mem := &mailer.Memory{}
	runKind(t, s, sales.OrderLinksJob, s.OrderLinks(mem))
	if len(mem.Sent) != 1 {
		t.Fatalf("%d emails, want 1", len(mem.Sent))
	}
	sent := mem.Sent[0]
	fresh := sales.OrderTokenAt(s.SecretKey, order, 1)
	// Sent to the address on the order, not to the spelling typed in the form.
	if sent.To != "buyer@example.com" || !strings.Contains(sent.Text, code+": https://tickets.example/orders/"+code+"?t="+fresh) {
		t.Fatalf("email %+v", sent)
	}
	if _, err := s.Order(ctx, code, old); !errors.Is(err, errNotFound) {
		t.Fatalf("old link still opens the order: %v", err)
	}
	if _, err := s.Order(ctx, code, fresh); err != nil {
		t.Fatalf("new link: %v", err)
	}
}

func TestOrderLinkAgainLooksTheSameForStrangers(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	drain(t, s, &mailer.Memory{})
	jobsQueued := func() int { return f.count(t, `select count(*) from jobs where kind = 'order_links'`) }

	for _, email := range []string{"nobody@example.com", "buyer@example.com"} {
		if err := s.RequestOrderLinks(ctx, "acme", "gala", email); err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		if n := jobsQueued(); n != 1 {
			t.Fatalf("%s: %d order_links jobs, want 1", email, n)
		}
		f.exec(t, `delete from jobs where kind = 'order_links'`)
	}

	f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "nobody@example.com"))
	mem := &mailer.Memory{}
	runKind(t, s, sales.OrderLinksJob, s.OrderLinks(mem))
	if len(mem.Sent) != 0 || f.count(t, `select link_version from orders where id = $1`, order) != 0 {
		t.Fatalf("a stranger's request sent %d emails or rotated a link", len(mem.Sent))
	}

	// A draft event's slug is taken the same way and sends nothing.
	f.exec(t, `update events set status = 'draft' where id = $1`, f.event)
	f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "buyer@example.com"))
	if n := jobsQueued(); n != 1 {
		t.Fatalf("draft: %d order_links jobs, want 1", n)
	}
	runKind(t, s, sales.OrderLinksJob, s.OrderLinks(mem))
	if len(mem.Sent) != 0 || f.count(t, `select link_version from orders where id = $1`, order) != 0 {
		t.Fatalf("a draft event's request sent %d emails or rotated a link", len(mem.Sent))
	}

	if err := s.RequestOrderLinks(ctx, "acme", "gala", "not-an-email"); invalidCode(err) != "bad_email" {
		t.Fatalf("not-an-email: %v, want bad_email", err)
	}
}

// failFirst fails its first Send, then delivers.
type failFirst struct {
	mailer.Memory
	failed bool
}

func (m *failFirst) Send(ctx context.Context, to, subject, text string) error {
	if !m.failed {
		m.failed = true
		return errors.New("mail server down")
	}
	return m.Memory.Send(ctx, to, subject, text)
}

func TestOrderLinkAgainRotatesOncePerTenMinutes(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	drain(t, s, &mailer.Memory{})
	version := func() int { return f.count(t, `select link_version from orders where id = $1`, order) }
	mem := &mailer.Memory{}
	for range 2 {
		f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "buyer@example.com"))
		runKind(t, s, sales.OrderLinksJob, s.OrderLinks(mem))
	}
	if len(mem.Sent) != 1 || version() != 1 {
		t.Fatalf("two requests in 10 minutes: %d emails, version %d; want 1 and 1", len(mem.Sent), version())
	}
	f.exec(t, `update orders set link_sent_at = now() - interval '11 minutes' where id = $1`, order)
	f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "buyer@example.com"))
	runKind(t, s, sales.OrderLinksJob, s.OrderLinks(mem))
	if len(mem.Sent) != 2 || version() != 2 {
		t.Fatalf("after 11 minutes: %d emails, version %d; want 2 and 2", len(mem.Sent), version())
	}

	// The first attempt rotates and then fails to send; its retry must still
	// send, though the link was rotated a moment ago.
	f.exec(t, `update orders set link_sent_at = now() - interval '11 minutes' where id = $1`, order)
	flaky := &failFirst{}
	w := &jobs.Worker{Pool: s.Pool, Handlers: map[string]jobs.Handler{sales.OrderLinksJob: s.OrderLinks(flaky)}}
	f.must(t, s.RequestOrderLinks(ctx, "acme", "gala", "buyer@example.com"))
	for range 2 {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `update jobs set run_at = now() where kind = 'order_links'`)
	}
	if len(flaky.Sent) != 1 || f.count(t, `select count(*) from jobs where kind = 'order_links'`) != 0 {
		t.Fatalf("retry after a failed send: %d emails, want 1", len(flaky.Sent))
	}
	code := f.str(t, `select code from orders where id = $1`, order)
	if !strings.Contains(flaky.Sent[0].Text, "?t="+sales.OrderTokenAt(s.SecretKey, order, version())) {
		t.Fatalf("the retry's email doesn't carry the live link: %q", flaky.Sent[0].Text)
	}
	if _, err := s.Order(ctx, code, sales.OrderTokenAt(s.SecretKey, order, version())); err != nil {
		t.Fatalf("live link: %v", err)
	}
}

func TestOrderPageShowsRefundedTickets(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true})
	drain(t, s, &mailer.Memory{})
	code := f.str(t, `select code from orders where id = $1`, order)
	o, err := s.Order(ctx, code, sales.OrderToken(s.SecretKey, order))
	f.must(t, err)
	if o.Status != "partially_refunded" || o.RefundedCents != 2600 || o.ReturnedCents != 0 || o.Event.Status != "live" {
		t.Fatalf("order %s, refunded %d, returned %d, event %s", o.Status, o.RefundedCents, o.ReturnedCents, o.Event.Status)
	}
	if v := o.Tickets[0]; v.Status != "void" || !v.Refunded || v.QR != "" {
		t.Fatalf("refunded ticket %+v", v)
	}
	if v := o.Tickets[1]; v.Status != "valid" || v.Refunded || len(v.QR) != 22 {
		t.Fatalf("kept ticket %+v", v)
	}
}

// A refund the buyer's bank sent back says so, on the order page and to the
// organizer, and no longer counts as refunded.
func TestABankReturnedRefundIsMarked(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	id := *f.refund(t, s, order, sales.RefundRequest{FeeBack: true}).RefundID
	drain(t, s, &mailer.Memory{})
	failRefund(t, s, f, id, "stripe_failed")
	if n := f.count(t, `select count(*) from refunds where id = $1 and returned_by_bank`, id); n != 1 {
		t.Fatal("a succeeded refund that failed isn't marked returned by the bank")
	}
	code := f.str(t, `select code from orders where id = $1`, order)
	o, err := s.Order(ctx, code, sales.OrderToken(s.SecretKey, order))
	f.must(t, err)
	if o.RefundedCents != 0 || o.ReturnedCents != 2600 {
		t.Fatalf("refunded %d, returned %d; want 0 and 2600", o.RefundedCents, o.ReturnedCents)
	}
	var v sales.RefundView
	asOwner(t, s, f, func(tx pgx.Tx) (err error) { v, err = sales.RefundByID(ctx, tx, id); return })
	if !v.ReturnedByBank {
		t.Fatal("the organizer's refund row doesn't say the bank sent it back")
	}
	// A refund that fails before going through was never returned.
	other, _, _ := f.paidOrder(t, s, fake, 1, 2)
	second := *f.refund(t, s, other, sales.RefundRequest{}).RefundID
	failRefund(t, s, f, second, "stripe_failed")
	if n := f.count(t, `select count(*) from refunds where id = $1 and returned_by_bank`, second); n != 0 {
		t.Fatal("a refund that never went through is marked returned by the bank")
	}
}

func failRefund(t *testing.T, s *sales.Service, f fixture, id uuid.UUID, failure string) {
	t.Helper()
	ctx := context.Background()
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FailRefundForTest(s, ctx, tx, f.org, id, failure) }))
}

// A late payment for an event that was cancelled meanwhile is refunded
// because of the cancellation, and the email says so.
func TestLatePaymentOnACancelledEventSaysCancelled(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	c, pi := paying(t, s, f, fake, sales.Details{Email: "late@example.com"}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.exec(t, `update events set status = 'cancelled' where id = $1`, f.event)
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	m := &mailer.Memory{}
	drain(t, s, m)
	for _, e := range m.Sent {
		if strings.HasPrefix(e.Subject, "Your refund") {
			if !strings.Contains(e.Text, "Gala was cancelled") || strings.Contains(e.Text, "weren't available") {
				t.Fatalf("refund email %q", e.Text)
			}
			return
		}
	}
	t.Fatalf("no refund email in %+v", m.Sent)
}
