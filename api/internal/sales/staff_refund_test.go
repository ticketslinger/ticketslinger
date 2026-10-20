package sales_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// refundAction records a refund_event action on f's event, in the staff
// session of org's latest suspension, as staff_act would.
func (f fixture) refundAction(t *testing.T, org uuid.UUID) uuid.UUID {
	t.Helper()
	return f.id(t, `insert into staff_actions(staff_session_id, user_id, organizer_id, kind, event_id, reason)
		select staff_session_id, user_id, organizer_id, 'refund_event', $2, 'Refunding every buyer of it'
		from staff_actions where organizer_id = $1 and kind = 'suspend' order by at desc limit 1 returning id`, org, f.event)
}

const alertTo = "alerts@example.com"

// finishAlert is the text of the refund-finished alert, "" if none was queued.
func (f fixture) finishAlert(t *testing.T) (subject, text string) {
	t.Helper()
	err := f.owner.QueryRow(context.Background(), `select payload->>'subject', payload->>'text' from jobs
		where kind = 'send_email' and payload->>'to' = $1`, alertTo).Scan(&subject, &text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ""
	}
	f.must(t, err)
	return subject, text
}

func pgMessage(err error) string {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pg.Message
	}
	return ""
}

// Spec test 28: two paid orders, one pending, one free. Each paid buyer gets
// everything back, our fee included; the pending payment is stopped; the
// free tickets are voided; the event stays live (it's off sale anyway).
func TestRefundEventForStaff(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	_, piA, _ := f.paidOrder(t, s, fake, 1, 1)
	_, piB, _ := f.paidOrder(t, s, fake, 2, 2)
	pending, piC := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	free := f.ticketType(t, f.org, f.event, "Free", 0, false)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, free)
	c := f.open(t, s, buy(sales.Item{TicketTypeID: free, Quantity: 1}))
	_, err := s.Pay(ctx, c.Token, sales.Details{Email: "free@example.com"})
	f.must(t, err)
	freeOrder, _, _ := f.orderOf(t, c.ID)

	f.suspend(t, f.org)
	action := f.refundAction(t, f.org)
	res, err := s.RefundEventForStaff(ctx, f.org, action, alertTo)
	f.must(t, err)
	if res.Orders != 2 || res.AmountCents != 2600+5200 || res.FeeCents != 100+200 || res.PendingOrders != 1 {
		t.Fatalf("result %+v", res)
	}
	subject, text := f.finishAlert(t)
	if subject != "Refund finished on acme" {
		t.Fatalf("alert subject %q", subject)
	}
	for _, want := range []string{"Gala (acme)", "Orders refunded: 2", "Back to buyers: $78.00", "Our fee returned: $3.00", "Pending payments stopped: 1", action.String()} {
		if !strings.Contains(text, want) {
			t.Errorf("the alert doesn't say %q: %s", want, text)
		}
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	if amount, fee := fake.Refunded(piA); amount != 2600 || fee != 100 {
		t.Fatalf("A: %d %d", amount, fee)
	}
	if amount, fee := fake.Refunded(piB); amount != 5200 || fee != 200 {
		t.Fatalf("B: %d %d", amount, fee)
	}
	if !fake.Cancelled(piC.ID) {
		t.Fatal("the pending payment wasn't stopped")
	}
	if _, _, st := f.orderOf(t, pending.ID); st != "cancelled" {
		t.Fatalf("pending order %s, want cancelled", st)
	}
	if n := f.count(t, `select count(*) from refunds where reason = 'platform' and fee_refunded and actor_user_id is null`); n != 2 {
		t.Fatalf("%d platform refunds with the fee back, want 2", n)
	}
	if n := f.count(t, `select count(*) from tickets where order_id = $1 and status = 'void'`, freeOrder); n != 1 {
		t.Fatal("the free order's ticket isn't void")
	}
	if n := f.count(t, `select count(*) from tickets where status = 'valid'`); n != 0 {
		t.Fatalf("%d valid tickets left", n)
	}
	if n := f.count(t, `select count(*) from events where id = $1 and status = 'live'`, f.event); n != 1 {
		t.Fatal("the event's status changed")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'event.refunded_by_support' and actor_system and entity_id = $1
		and after = jsonb_build_object('staff_action_id', $2::text, 'orders', 2, 'amount_cents', 7800, 'fee_cents', 300, 'pending_orders', 1, 'failed_orders', '[]'::jsonb)`,
		f.event, action); n != 1 {
		t.Fatal("no event.refunded_by_support row with the totals")
	}
	if n := f.count(t, `select count(*) from staff_actions where id = $1 and finished_at is not null
		and result = '{"orders": 2, "amount_cents": 7800, "fee_cents": 300, "pending_orders": 1, "failed_orders": []}'`, action); n != 1 {
		t.Fatal("the action isn't finished with the totals")
	}
	sent := 0
	for _, e := range m.Sent {
		if strings.HasPrefix(e.Subject, "Your refund") {
			sent++
			if !strings.Contains(e.Text, "Gala isn't being sold through TicketSlinger any more") {
				t.Errorf("refund email: %s", e.Text)
			}
		}
	}
	if sent != 2 {
		t.Fatalf("%d refund emails, want 2", sent)
	}
}

// A double submit: one refund per order, and the other call is told it's done.
func TestRefundEventOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	f.paidOrder(t, s, fake, 1, 1)
	f.suspend(t, f.org)
	action := f.refundAction(t, f.org)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = s.RefundEventForStaff(ctx, f.org, action, alertTo) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || pgMessage(errors.Join(errs...)) != "already_done" {
		t.Fatalf("%v, want one success and one already_done", errs)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 1 {
		t.Fatalf("%d refunds, want 1", n)
	}
}

func TestRefundEventNeedsSuspension(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	f.paidOrder(t, s, fake, 1, 1)
	f.suspend(t, f.org)
	action := f.refundAction(t, f.org)
	f.lift(t, f.org)
	if _, err := s.RefundEventForStaff(context.Background(), f.org, action, alertTo); pgMessage(err) != "not_suspended" {
		t.Fatalf("%v, want not_suspended", err)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 0 {
		t.Fatalf("%d refunds", n)
	}
}

// A refund that fails part way leaves nothing behind and the action
// unfinished, so running it again refunds each order exactly once.
func TestRefundEventRetriesAfterAFailure(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	f.paidOrder(t, s, fake, 1, 1)
	f.paidOrder(t, s, fake, 1, 2)
	f.suspend(t, f.org)
	action := f.refundAction(t, f.org)
	// Fails on the last write, after both refunds are made.
	f.exec(t, `create function fail_support_refund() returns trigger language plpgsql as $$
		begin raise exception 'simulated failure'; end $$`)
	f.exec(t, `create trigger fail_support_refund before insert on audit_log for each row
		when (new.action = 'event.refunded_by_support') execute function fail_support_refund()`)
	if _, err := s.RefundEventForStaff(ctx, f.org, action, alertTo); pgMessage(err) != "simulated failure" {
		t.Fatalf("%v, want the simulated failure", err)
	}
	if n := f.count(t, `select (select count(*) from refunds) + (select count(*) from jobs where kind = 'refund')
		+ (select count(*) from staff_actions where finished_at is not null) + (select count(*) from jobs where payload->>'to' = $1)`, alertTo); n != 0 {
		t.Fatal("a failed refund left something behind")
	}
	f.exec(t, `drop trigger fail_support_refund on audit_log`)
	res, err := s.RefundEventForStaff(ctx, f.org, action, alertTo)
	f.must(t, err)
	if res.Orders != 2 {
		t.Fatalf("retry: %+v", res)
	}
	// Staff starting again with a new action find nothing left to refund.
	res, err = s.RefundEventForStaff(ctx, f.org, f.refundAction(t, f.org), alertTo)
	f.must(t, err)
	if res.Orders != 0 || res.AmountCents != 0 {
		t.Fatalf("a second action refunded again: %+v", res)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 2 {
		t.Fatalf("%d refunds, want 2", n)
	}
}

func TestPlatformRefundEmail(t *testing.T) {
	subject, text := sales.RefundEmailForTest("platform", false, true, "Gala", "Acme", 2600)
	if subject != "Your refund for Gala" || text != "Gala isn't being sold through TicketSlinger any more, so we've refunded you $26.00 in full. "+
		"It goes back to the card you paid with and usually shows up in 5 to 10 days." {
		t.Fatalf("%q %q", subject, text)
	}
	// Part of the order was refunded before: not "in full".
	if _, text := sales.RefundEmailForTest("platform", false, false, "Gala", "Acme", 2600); text != "Gala isn't being sold through TicketSlinger any more, "+
		"so we've refunded you $26.00. It goes back to the card you paid with and usually shows up in 5 to 10 days." {
		t.Fatalf("part refunded before: %q", text)
	}
}

// An order the organizer had partly refunded, keeping the fee: staff refund
// what's left, with its fee, once; the buyer isn't told "in full".
func TestRefundEventAfterAPartRefund(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
	f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1]})
	f.suspend(t, f.org)
	res, err := s.RefundEventForStaff(ctx, f.org, f.refundAction(t, f.org), alertTo)
	f.must(t, err)
	if res.Orders != 1 || res.AmountCents != 2600 || res.FeeCents != 100 {
		t.Fatalf("result %+v, want the one ticket left with its fee", res)
	}
	res, err = s.RefundEventForStaff(ctx, f.org, f.refundAction(t, f.org), alertTo)
	f.must(t, err)
	if res.Orders != 0 {
		t.Fatalf("second action: %+v", res)
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	if amount, fee := fake.Refunded(pi); amount != 2500+2600 || fee != 100 {
		t.Fatalf("refunded %d, fee %d", amount, fee)
	}
	if n := f.count(t, `select count(*) from orders where id = $1 and status = 'refunded'`, order); n != 1 {
		t.Fatal("the order isn't refunded")
	}
	for _, e := range m.Sent {
		if strings.Contains(e.Text, "TicketSlinger") && strings.Contains(e.Text, "in full") {
			t.Errorf("a part refund says in full: %s", e.Text)
		}
	}
}

// An order the database refuses (its payment has nothing left) doesn't
// hold up the others: they're refunded, and it's listed for a person.
func TestRefundEventListsARefusedOrder(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	stuck, _, _ := f.paidOrder(t, s, fake, 1, 1)
	_, piB, _ := f.paidOrder(t, s, fake, 1, 2)
	// Money already gone back outside the tickets: the payment has nothing left.
	f.exec(t, `insert into refunds(organizer_id, payment_id, amount_cents, fee_refunded, status, reason)
		select organizer_id, id, amount_cents, false, 'succeeded', 'organizer' from payments where order_id = $1`, stuck)
	f.suspend(t, f.org)
	action := f.refundAction(t, f.org)
	res, err := s.RefundEventForStaff(ctx, f.org, action, alertTo)
	f.must(t, err)
	if res.Orders != 1 || len(res.Failed) != 1 || res.Failed[0].Order != stuck || res.Failed[0].Reason != "payment_used_up" {
		t.Fatalf("result %+v", res)
	}
	want := `[{"reason": "payment_used_up", "order_id": "` + stuck.String() + `"}]`
	if n := f.count(t, `select count(*) from staff_actions where id = $1 and finished_at is not null and result->'failed_orders' = $2::jsonb`, action, want); n != 1 {
		t.Fatal("the action doesn't list the refused order")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'event.refunded_by_support' and after->'failed_orders' = $1::jsonb`, want); n != 1 {
		t.Fatal("the audit row doesn't list the refused order")
	}
	if subject, text := f.finishAlert(t); subject != "Refund finished on acme, 1 order to refund by hand" || !strings.Contains(text, stuck.String()+": payment_used_up") {
		t.Fatalf("alert %q: %s", subject, text)
	}
	if n := f.count(t, `select count(*) from tickets t join orders o on o.id = t.order_id where o.id = $1 and t.status = 'valid'`, stuck); n != 1 {
		t.Fatal("the refused order's ticket changed")
	}
	drain(t, s, &mailer.Memory{})
	if amount, _ := fake.Refunded(piB); amount != 2600 {
		t.Fatalf("B refunded %d", amount)
	}
}
