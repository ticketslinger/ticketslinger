package sales_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// asOwner runs fn in the owner's own organizer transaction, as the API does.
func asOwner(t *testing.T, s *sales.Service, f fixture, fn func(tx pgx.Tx) error) {
	t.Helper()
	f.must(t, db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error { return fn(tx) }))
}

func (f fixture) refund(t *testing.T, s *sales.Service, order uuid.UUID, in sales.RefundRequest) (res sales.RefundResult) {
	t.Helper()
	asOwner(t, s, f, func(tx pgx.Tx) (err error) {
		res, err = sales.RefundTickets(context.Background(), tx, f.org, f.event, order, in, "organizer", audit.Actor{User: f.user})
		return
	})
	return res
}

func (f fixture) retry(s *sales.Service, order, refund uuid.UUID) (v sales.RefundView, err error) {
	err = db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		v, err = sales.RetryRefund(context.Background(), tx, f.org, f.event, order, refund, audit.Actor{User: f.user})
		return
	})
	return v, err
}

func (f fixture) quotaUsed(t *testing.T) (n int) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), `select quota_used($1, null)`, f.hall).Scan(&n))
	return n
}

func refundEmails(m *mailer.Memory) (n int) {
	for _, e := range m.Sent {
		if strings.HasPrefix(e.Subject, "Your refund") {
			n++
		}
	}
	return n
}

// The refund money rule, to the cent, for every mode.
func TestRefundAmountsAreExact(t *testing.T) {
	for name, tc := range map[string]struct {
		feeMode    string
		feeBack    bool
		one        bool // refund one ticket of two, else the whole order
		wantAmount int64
		wantFee    int64
	}{
		"pass, whole order, fee back":  {"pass", true, false, 5200, 200},
		"pass, whole order, fee kept":  {"pass", false, false, 5000, 0},
		"pass, one ticket, fee back":   {"pass", true, true, 2600, 100},
		"pass, one ticket, fee kept":   {"pass", false, true, 2500, 0},
		"absorb, one ticket, fee back": {"absorb", true, true, 2500, 100},
		"absorb, one ticket, fee kept": {"absorb", false, true, 2500, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
			f.exec(t, `update events set fee_mode = $2 where id = $1`, f.event, tc.feeMode)
			order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
			in := sales.RefundRequest{FeeBack: tc.feeBack}
			if tc.one {
				in.TicketIDs = tickets[:1]
			}
			res := f.refund(t, s, order, in)
			if res.AmountCents != tc.wantAmount || res.FeeCents != tc.wantFee || res.RefundID == nil {
				t.Fatalf("result %+v", res)
			}
			drain(t, s, &mailer.Memory{})
			if amount, fee := fake.Refunded(pi); amount != tc.wantAmount || fee != tc.wantFee {
				t.Fatalf("Stripe refunded %d and fee %d", amount, fee)
			}
			want := "refunded"
			if tc.one {
				want = "partially_refunded"
			}
			if res.OrderStatus != want {
				t.Fatalf("order %s, want %s", res.OrderStatus, want)
			}
			if n := f.count(t, `select count(*) from audit_log where action = 'order.refunded' and actor_user_id = $1 and entity_id = $2
				and (after->>'amount_cents')::bigint = $3`, f.user, order, tc.wantAmount); n != 1 {
				t.Fatalf("%d order.refunded rows, want 1", n)
			}
		})
	}
}

func TestTwoRefundsOfOneTicketMakeOne(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	ctx := context.Background()
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			errs[i] = db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
				_, err := sales.RefundTickets(ctx, tx, f.org, f.event, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true}, "organizer", audit.Actor{User: f.user})
				return err
			})
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case invalidCode(err) != "bad_tickets" || fieldOf(err) != "ticket_ids":
			t.Fatalf("second refund: %v, want bad_tickets", err)
		}
	}
	if ok != 1 || f.count(t, `select count(*) from refunds`) != 1 {
		t.Fatalf("%d succeeded, %d refund rows; want 1 and 1", ok, f.count(t, `select count(*) from refunds`))
	}
	// And the whole order, twice in a row: the second has nothing left.
	f.refund(t, s, order, sales.RefundRequest{})
	err := db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		_, err := sales.RefundTickets(ctx, tx, f.org, f.event, order, sales.RefundRequest{}, "organizer", audit.Actor{User: f.user})
		return err
	})
	if conflictCode(err) != "not_refundable" {
		t.Fatalf("refunding a refunded order: %v", err)
	}
}

// A ticket id from another order, or one that doesn't exist, refunds nothing.
func TestRefundNamesOnlyThisOrdersTickets(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	_, _, others := f.paidOrder(t, s, fake, 1, 2)
	for _, id := range []uuid.UUID{others[0], uuid.New()} {
		err := db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
			_, err := sales.RefundTickets(context.Background(), tx, f.org, f.event, order, sales.RefundRequest{TicketIDs: []uuid.UUID{id}}, "organizer", audit.Actor{User: f.user})
			return err
		})
		if invalidCode(err) != "bad_tickets" {
			t.Fatalf("refunding ticket %s: %v, want bad_tickets", id, err)
		}
	}
	err := db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		_, err := sales.RefundTickets(context.Background(), tx, f.org, uuid.New(), order, sales.RefundRequest{}, "organizer", audit.Actor{User: f.user})
		return err
	})
	if !errors.Is(err, errNotFound) {
		t.Fatalf("order under another event: %v, want not found", err)
	}
	if n := f.count(t, `select count(*) from refunds`) + f.count(t, `select count(*) from tickets where status = 'void'`); n != 0 {
		t.Fatalf("%d refunds and void tickets, want none", n)
	}
}

func TestFreeTicketsVoidAndReleaseAtOnce(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 0})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	_, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "free@example.com"})
	f.must(t, err)
	order, _, _ := f.orderOf(t, c.ID)
	res := f.refund(t, s, order, sales.RefundRequest{})
	if res.RefundID != nil || res.OrderStatus != "cancelled" || f.count(t, `select count(*) from tickets where released_at is not null`) != 1 {
		t.Fatalf("free cancellation %+v", res)
	}
	if f.quotaUsed(t) != 0 {
		t.Fatalf("quota used %d, want 0", f.quotaUsed(t))
	}
}

// "Try again" sends the same refund row again under its own key family:
// no second row, the tickets stay where they are, and Stripe gets one refund.
func TestRetryRequeuesTheSameRefund(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
	res := f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true})
	id := *res.RefundID
	if _, err := f.retry(s, order, id); conflictCode(err) != "not_failed" {
		t.Fatalf("retrying a requested refund: %v, want not_failed", err)
	}
	crashPark(t, s, f)
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'gave_up'`, id); n != 1 {
		t.Fatal("not given up")
	}
	v, err := f.retry(s, order, id)
	f.must(t, err)
	if v.ID != id || v.Status != "requested" || v.Failure != nil || v.AmountCents != 2600 || v.FeeCents != 100 || !v.FeeBack {
		t.Fatalf("retried refund %+v", v)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 1 {
		t.Fatalf("%d refund rows, want the one", n)
	}
	if n := f.count(t, `select count(*) from tickets where id = $1 and refund_id = $2 and status = 'void'`, tickets[0], id); n != 1 {
		t.Fatal("the ticket moved off its refund")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.retried' and entity_type = 'refund' and entity_id = $1
		and actor_user_id = $2`, id, f.user); n != 1 {
		t.Fatalf("%d refund.retried rows, want 1", n)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'refund' and payload->>'refund_id' = $1 and run_at <> 'infinity'`, id.String()); n != 1 {
		t.Fatalf("%d refund jobs queued, want 1", n)
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	in := fake.Refunds()
	if len(in) != 1 || in[0].IdempotencyKey != "refund-"+id.String()+"-0" || in[0].AmountCents != 2600 {
		t.Fatalf("Stripe asked for %+v, want one refund under refund-%s-0", in, id)
	}
	if amount, fee := fake.Refunded(pi); amount != 2600 || fee != 100 {
		t.Fatalf("Stripe refunded %d and fee %d", amount, fee)
	}
	if f.quotaUsed(t) != 1 || refundEmails(m) != 1 {
		t.Fatalf("quota used %d, %d refund emails; want 1 and 1", f.quotaUsed(t), refundEmails(m))
	}
	if _, err := f.retry(s, order, id); conflictCode(err) != "not_failed" {
		t.Fatalf("retrying a succeeded refund: %v, want not_failed", err)
	}
	if _, err := f.retry(s, order, uuid.New()); !errors.Is(err, errNotFound) {
		t.Fatalf("retrying an unknown refund: %v, want not found", err)
	}
	other, _, _ := f.paidOrder(t, s, fake, 1, 2)
	if _, err := f.retry(s, other, id); !errors.Is(err, errNotFound) {
		t.Fatalf("retrying a refund under another order: %v, want not found", err)
	}
}

// Stripe said failed: no money went out under that key, so the retry moves
// to the next one (Stripe would replay the failure under the old key).
func TestRetryAfterStripeSaidFailedUsesTheNextKey(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	id := *f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1]}).RefundID
	f.exec(t, `update refunds set stripe_refund_id = 're_failed' where id = $1`, id)
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FinishRefundForTest(s, ctx, tx, f.org, id, "failed") }))
	_, err := f.retry(s, order, id)
	f.must(t, err)
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'requested' and attempt = 1 and stripe_refund_id is null`, id); n != 1 {
		t.Fatal("a refund Stripe failed isn't on its next key")
	}
	drain(t, s, &mailer.Memory{})
	if in := fake.Refunds(); len(in) != 1 || in[0].IdempotencyKey != "refund-"+id.String()+"-1" {
		t.Fatalf("Stripe asked for %+v, want refund-%s-1", in, id)
	}
	if f.quotaUsed(t) != 1 {
		t.Fatalf("quota used %d, want 1", f.quotaUsed(t))
	}
}

// We gave up, the organizer pressed "Try again", and Stripe's late word on
// the first try lands too, before or after the retry runs. The buyer is
// refunded once, the seat comes back once and one email goes out.
func TestALateSuccessAndARetryPayOnce(t *testing.T) {
	for name, webhookFirst := range map[string]bool{"webhook first": true, "retry first": false} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
			ctx := context.Background()
			order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
			id := *f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true}).RefundID
			// Stripe took the first try, but its answer never reached us.
			_, err := fake.Refund(ctx, plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi, AmountCents: 2600,
				IdempotencyKey: "refund-" + id.String() + "-0"})
			f.must(t, err)
			crashPark(t, s, f)
			if n := f.count(t, `select count(*) from refunds where id = $1 and failure = 'gave_up'`, id); n != 1 {
				t.Fatal("not given up")
			}
			_, err = f.retry(s, order, id)
			f.must(t, err)
			late := func() {
				f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error {
					return sales.FinishRefundForTest(s, ctx, tx, f.org, id, "succeeded")
				}))
			}
			m := &mailer.Memory{}
			if webhookFirst {
				late()
				drain(t, s, m)
			} else {
				drain(t, s, m)
				late()
				drain(t, s, m)
			}
			if n := len(fake.Refunds()); n != 1 {
				t.Fatalf("%d Stripe refunds, want 1", n)
			}
			if amount, fee := fake.Refunded(pi); amount != 2600 || fee != 100 {
				t.Fatalf("Stripe refunded %d and fee %d", amount, fee)
			}
			if n := f.count(t, `select count(*) from tickets where released_at is not null`); n != 1 || f.quotaUsed(t) != 1 {
				t.Fatalf("%d tickets released, quota used %d; want 1 and 1", n, f.quotaUsed(t))
			}
			if refundEmails(m) != 1 {
				t.Fatalf("%d refund emails, want 1", refundEmails(m))
			}
			if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded' and fee_refunded_at is not null`, id); n != 1 {
				t.Fatal("the refund didn't finish with its fee back")
			}
		})
	}
}

// refund.fee_stuck is written once per refund, and not at all while another
// job for the refund is still due to try the fee again.
func TestFeeStuckIsWrittenOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	_, order, _ := latePaid(t, s, f, fake, "late@example.com")
	refund, payload := refundOf(t, f, order)
	fake.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Refused: true, Status: 400})
	drain(t, s, &mailer.Memory{}) // the refund succeeds; the fee step fails once and waits to try again
	stuck := func() int {
		return f.count(t, `select count(*) from audit_log where action = 'refund.fee_stuck' and entity_id = $1`, refund)
	}
	parked := sales.RefundParked(s)
	parked(ctx, payload, "a twin job gave up")
	if stuck() != 0 {
		t.Fatal("fee_stuck written while another job will still try the fee")
	}
	f.exec(t, `update jobs set run_at = 'infinity' where kind = 'refund'`)
	parked(ctx, payload, "gave up")
	parked(ctx, payload, "gave up again")
	if stuck() != 1 {
		t.Fatalf("%d fee_stuck rows, want 1", stuck())
	}
}

// The order list counts valid tickets only, and says what went back.
func TestListOrdersCountsWhatsLeft(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true})
	drain(t, s, &mailer.Memory{})
	var list []sales.OrderSummary
	asOwner(t, s, f, func(tx pgx.Tx) (err error) {
		list, err = sales.ListOrders(context.Background(), tx, f.event)
		return
	})
	if len(list) != 1 {
		t.Fatalf("%d orders", len(list))
	}
	if o := list[0]; o.TicketCount != 1 || o.RefundedCents != 2600 || o.FeeRefundedCents != 100 || o.Status != "partially_refunded" {
		t.Fatalf("order %+v", o)
	}
	var d sales.OrderDetailView
	asOwner(t, s, f, func(tx pgx.Tx) (err error) {
		d, err = sales.OrderDetail(context.Background(), tx, f.event, order)
		return
	})
	if d.ID != order || d.FeeMode != "pass" || len(d.Tickets) != 2 || len(d.Refunds) != 1 || d.Refunds[0].Status != "succeeded" ||
		d.Tickets[0].Status != "void" || d.Tickets[0].RefundID == nil || *d.Tickets[0].RefundID != d.Refunds[0].ID ||
		d.Tickets[1].Status != "valid" || d.Tickets[1].ChargeCents != 2500 || d.Tickets[1].FeeCents != 100 {
		t.Fatalf("detail %+v", d)
	}
}
