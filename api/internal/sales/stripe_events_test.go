package sales_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// ours is the metadata we send with a refund: the row and its attempt.
func ours(refund uuid.UUID, attempt int) map[string]string {
	return map[string]string{"refund_id": refund.String(), "attempt": strconv.Itoa(attempt)}
}

// refundEvent is the refund.updated Stripe sends for refund stripeID, as
// event evt_r<n>. Two deliveries of one event share n.
func refundEvent(stripeID, pi, account string, amount int64, status string, md map[string]string, n int) stripe.Event {
	obj := map[string]any{"id": stripeID, "object": "refund", "amount": amount, "payment_intent": pi, "status": status}
	if md != nil {
		obj["metadata"] = md
	}
	raw, _ := json.Marshal(obj)
	return stripe.Event{ID: fmt.Sprintf("evt_r%d", n), Type: stripe.EventTypeRefundUpdated, Account: account, Data: &stripe.EventData{Raw: raw}}
}

// pendingRefund is a paid two-ticket order with its first ticket refunded,
// fee back (2600), and Stripe answering pending: the row is requested with
// Stripe's id stored, and the seat still held.
func pendingRefund(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake) (order uuid.UUID, pi string, ticket, refund uuid.UUID, stripeID string) {
	t.Helper()
	order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
	refund = *f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true}).RefundID
	fake.RefundsPending(true)
	drain(t, s, &mailer.Memory{})
	fake.RefundsPending(false)
	var id *string
	f.must(t, f.owner.QueryRow(context.Background(), `select stripe_refund_id from refunds where id = $1 and status = 'requested'`, refund).Scan(&id))
	if id == nil || f.quotaUsed(t) != 2 {
		t.Fatalf("pending refund: Stripe id %v, quota used %d; want an id and 2", id, f.quotaUsed(t))
	}
	return order, pi, tickets[0], refund, *id
}

// rowState is everything a refund event could change about a refund.
func (f fixture) rowState(t *testing.T, refund uuid.UUID) string {
	t.Helper()
	var s string
	f.must(t, f.owner.QueryRow(context.Background(), `select r.status || '/' || coalesce(r.failure, '') || '/' || coalesce(r.stripe_refund_id, '') || '/' ||
			r.attempt || '/' || (r.fee_refunded_at is not null) || '/' ||
			(select count(*) from tickets where refund_id = r.id and released_at is not null) || '/' ||
			(select count(*) from jobs where kind in ('refund', 'send_email') and run_at <> 'infinity') || '/' ||
			(select count(*) from audit_log where entity_id = r.id)
		from refunds r where r.id = $1`, refund).Scan(&s))
	return s
}

// Organizer A took a payment on acct_fake1, then disconnected, and B linked
// the same account. The payment's late webhook still settles A's order.
func TestMovedAccountLatePaymentStillSettles(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.exec(t, `update organizers set stripe_disconnected_at = now(), stripe_charges_enabled = false where id = $1`, f.org)
	b := f.organizer(t, "bravo")
	f.exec(t, `update organizers set stripe_account_id = 'acct_fake1' where id = $1`, b)
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	if _, _, st := f.orderOf(t, c.ID); st != "paid" {
		t.Fatalf("A's order %s, want paid", st)
	}
	if n := f.count(t, `select count(*) from stripe_events`); n != 1 {
		t.Fatalf("%d events recorded", n)
	}
}

// The same move, but the hold ran out and the seat went to someone else:
// A's late payment is cancelled and refunded on the account it was paid on,
// so the buyer is never left charged with no order.
func TestMovedAccountLatePaymentThatCantBeSoldIsRefunded(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	expire(t, f, c.ID)
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.exec(t, `update organizers set stripe_disconnected_at = now(), stripe_charges_enabled = false where id = $1`, f.org)
	b := f.organizer(t, "bravo")
	f.exec(t, `update organizers set stripe_account_id = 'acct_fake1' where id = $1`, b)
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	order, _, st := f.orderOf(t, c.ID)
	if st != "cancelled" {
		t.Fatalf("A's order %s, want cancelled", st)
	}
	if n := f.count(t, `select count(*) from refunds r join payments p on p.id = r.payment_id
		where p.order_id = $1 and r.organizer_id = $2 and r.reason = 'late_payment'`, order, f.org); n != 1 {
		t.Fatalf("%d late refunds on A's order, want 1", n)
	}
	drain(t, s, &mailer.Memory{})
	if in := fake.Refunds(); len(in) != 1 || in[0].Account != "acct_fake1" || in[0].PaymentIntentID != pi.ID || in[0].AmountCents != c.Quote.TotalCents {
		t.Fatalf("refunds asked for %+v", in)
	}
}

func TestRefundWebhookFinishesTheRefund(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	_, pi, ticket, refund, stripeID := pendingRefund(t, s, f, fake)
	ev := refundEvent(stripeID, pi, "acct_fake1", 2600, "succeeded", ours(refund, 0), 1)
	f.must(t, s.HandleStripeEvent(ctx, ev))
	again := ev
	again.ID = "evt_r2" // refund.created and refund.updated can both say succeeded
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, e := range []stripe.Event{ev, again} {
		wg.Go(func() { errs[i] = s.HandleStripeEvent(ctx, e) })
	}
	wg.Wait()
	for _, err := range errs {
		f.must(t, err)
	}
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded' and failure is null`, refund); n != 1 {
		t.Fatal("the refund isn't succeeded")
	}
	if n := f.count(t, `select count(*) from tickets where id = $1 and released_at is not null`, ticket); n != 1 || f.quotaUsed(t) != 1 {
		t.Fatalf("ticket released %d, quota used %d; want 1 and 1", n, f.quotaUsed(t))
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email' and payload->>'subject' = 'Your refund for Gala'`); n != 1 {
		t.Fatalf("%d refund emails queued, want 1", n)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'refund' and payload->>'refund_id' = $1`, refund.String()); n != 1 {
		t.Fatalf("%d fee steps queued, want 1", n)
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	drain(t, s, m)
	if fr := fake.FeeRefunds(); len(fr) != 1 || fr[0].AmountCents != 100 {
		t.Fatalf("fee refunds %+v, want one of 100", fr)
	}
	if n := f.count(t, `select count(*) from refunds where id = $1 and fee_refunded_at is not null`, refund); n != 1 || refundEmails(m) != 1 {
		t.Fatalf("fee marked %d, %d refund emails; want 1 and 1", n, refundEmails(m))
	}
}

// Stripe failed a refund it had called pending: the organizer sees it
// failed, and the seat stays held until a retry succeeds.
func TestRefundStripeFailsIsShown(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	_, pi, _, refund, stripeID := pendingRefund(t, s, f, fake)
	ev := refundEvent(stripeID, pi, "acct_fake1", 2600, "failed", ours(refund, 0), 1)
	ev.Type = stripe.EventTypeRefundFailed
	f.must(t, s.HandleStripeEvent(context.Background(), ev))
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'stripe_failed'`, refund); n != 1 {
		t.Fatal("the refund isn't failed with stripe_failed")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed' and entity_id = $1`, refund); n != 1 || f.quotaUsed(t) != 2 {
		t.Fatalf("%d refund.failed rows, quota used %d; want 1 and 2", n, f.quotaUsed(t))
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 0 {
		t.Fatalf("%d emails queued for a failed refund", n)
	}
}

// We gave up asking (the answer was lost), but Stripe made the refund: its
// event finds the row by metadata, stores the id and finishes it, late.
func TestRefundWebhookFinishesAGaveUpRefund(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	order, pi, tickets := f.paidOrder(t, s, fake, 2, 1)
	refund := *f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true}).RefundID
	fake.LoseNextRefundAnswer()
	drain(t, s, &mailer.Memory{})
	crashPark(t, s, f)
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'gave_up' and stripe_refund_id is null`, refund); n != 1 {
		t.Fatal("not given up")
	}
	list, err := fake.ListRefunds(ctx, "acct_fake1", pi)
	f.must(t, err)
	f.must(t, s.HandleStripeEvent(ctx, refundEvent(list[0].ID, pi, "acct_fake1", 2600, "succeeded", ours(refund, 0), 1)))
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded' and stripe_refund_id = $2`, refund, list[0].ID); n != 1 {
		t.Fatal("Stripe's succeeded didn't finish the given-up refund")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.succeeded_late' and entity_id = $1`, refund); n != 1 || f.quotaUsed(t) != 1 {
		t.Fatalf("%d refund.succeeded_late rows, quota used %d; want 1 and 1", n, f.quotaUsed(t))
	}
}

// Stripe sometimes takes back a refund it first called succeeded.
func TestRefundThatFailsLaterIsShown(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	order, pi, ticket, refund, stripeID := pendingRefund(t, s, f, fake)
	f.must(t, s.HandleStripeEvent(ctx, refundEvent(stripeID, pi, "acct_fake1", 2600, "succeeded", ours(refund, 0), 1)))
	var before string
	f.must(t, f.owner.QueryRow(ctx, `select status from orders where id = $1`, order).Scan(&before))
	ev := refundEvent(stripeID, pi, "acct_fake1", 2600, "failed", ours(refund, 0), 9)
	ev.Type = stripe.EventTypeRefundFailed
	f.must(t, s.HandleStripeEvent(ctx, ev))
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'stripe_failed'`, refund); n != 1 {
		t.Fatal("the refund isn't failed with stripe_failed")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed' and actor_user_id is null and entity_id = $1`, refund); n != 1 {
		t.Fatalf("%d refund.failed system rows, want 1", n)
	}
	if n := f.count(t, `select count(*) from tickets where id = $1 and released_at is not null`, ticket); n != 1 {
		t.Fatal("the released ticket came back")
	}
	if n := f.count(t, `select count(*) from orders where id = $1 and status = $2`, order, before); n != 1 {
		t.Fatalf("the order moved from %s", before)
	}
}

func TestRefundWebhookWeDidntAskForChangesNothing(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 4, price: 2500})
	ctx := context.Background()
	_, pi, _, refund, stripeID := pendingRefund(t, s, f, fake)
	f.organizer(t, "bravo") // acct_fakebravo, live
	_, otherPI, _ := f.paidOrder(t, s, fake, 1, 2)
	state := f.rowState(t, refund)
	for i, c := range []struct {
		name     string
		ev       stripe.Event
		recorded bool
	}{
		{"made by hand in Stripe", refundEvent("re_byhand", pi, "acct_fake1", 2600, "succeeded", nil, 0), true},
		{"from an account we don't know", refundEvent(stripeID, pi, "acct_other", 2600, "succeeded", ours(refund, 0), 0), false},
		{"another organizer's account", refundEvent(stripeID, pi, "acct_fakebravo", 2600, "succeeded", ours(refund, 0), 0), true},
		{"another amount", refundEvent(stripeID, pi, "acct_fake1", 2599, "succeeded", ours(refund, 0), 0), true},
		{"another intent", refundEvent(stripeID, otherPI, "acct_fake1", 2600, "succeeded", ours(refund, 0), 0), true},
		{"another Stripe id than the one stored", refundEvent("re_other", pi, "acct_fake1", 2600, "succeeded", ours(refund, 0), 0), true},
	} {
		c.ev.ID = fmt.Sprintf("evt_x%d", i)
		f.must(t, s.HandleStripeEvent(ctx, c.ev))
		if got := f.rowState(t, refund); got != state {
			t.Errorf("%s: refund went %s -> %s", c.name, state, got)
		}
		if n := f.count(t, `select count(*) from stripe_events where id = $1`, c.ev.ID); (n == 1) != c.recorded {
			t.Errorf("%s: recorded %d, want %v", c.name, n, c.recorded)
		}
	}
	// acme moved to a new account: an event from it that names the payment
	// made on the old one reaches acme, and still isn't the payment's.
	f.exec(t, `update organizers set stripe_account_id = 'acct_fakenew' where id = $1`, f.org)
	ev := refundEvent(stripeID, pi, "acct_fakenew", 2600, "succeeded", ours(refund, 0), 99)
	f.must(t, s.HandleStripeEvent(ctx, ev))
	if got := f.rowState(t, refund); got != state || f.count(t, `select count(*) from stripe_events where id = 'evt_r99'`) != 1 {
		t.Errorf("acme's new account: refund went %s -> %s", state, got)
	}
}

// Stripe failed attempt 0, the organizer retried, and the row moved to
// attempt 1. Attempt 0's events, however late, never touch it again.
func TestAStaleEventNeverChangesTheRetry(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	order, pi, _, refund, re0 := pendingRefund(t, s, f, fake)
	failed := func(stripeID string, attempt, n int) stripe.Event {
		ev := refundEvent(stripeID, pi, "acct_fake1", 2600, "failed", ours(refund, attempt), n)
		ev.Type = stripe.EventTypeRefundFailed
		return ev
	}
	f.must(t, s.HandleStripeEvent(ctx, failed(re0, 0, 1)))
	_, err := f.retry(s, order, refund)
	f.must(t, err)
	want := f.rowState(t, refund)
	if !strings.HasPrefix(want, "requested///1/") {
		t.Fatalf("after the retry %s, want requested on attempt 1 with no Stripe id", want)
	}
	// No Stripe id stored yet: attempt 0's events don't match attempt 1.
	f.must(t, s.HandleStripeEvent(ctx, failed(re0, 0, 2)))
	f.must(t, s.HandleStripeEvent(ctx, failed("re_unseen", 0, 3)))
	if got := f.rowState(t, refund); got != want {
		t.Fatalf("a stale event moved the retry: %s, want %s", got, want)
	}
	fake.RefundsPending(true)
	drain(t, s, &mailer.Memory{})
	var re1 string
	f.must(t, f.owner.QueryRow(ctx, `select stripe_refund_id from refunds where id = $1 and status = 'requested' and attempt = 1`, refund).Scan(&re1))
	// With attempt 1's id stored, attempt 0's id doesn't match either.
	f.must(t, s.HandleStripeEvent(ctx, failed(re0, 0, 4)))
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'requested'`, refund); n != 1 {
		t.Fatal("a stale event failed the retry")
	}
	f.must(t, s.HandleStripeEvent(ctx, refundEvent(re1, pi, "acct_fake1", 2600, "succeeded", ours(refund, 1), 5)))
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded'`, refund); n != 1 || f.quotaUsed(t) != 1 {
		t.Fatalf("attempt 1's success: %d succeeded, quota used %d", n, f.quotaUsed(t))
	}
}

// charge.refunded carries no refund ids in this API version; it's recorded
// and cross-checked, never acted on.
func TestChargeRefundedIsOnlyChecked(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	_, pi, _, refund, stripeID := pendingRefund(t, s, f, fake)
	f.must(t, s.HandleStripeEvent(ctx, refundEvent(stripeID, pi, "acct_fake1", 2600, "succeeded", ours(refund, 0), 1)))
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	charge := func(refunded int64, n int) stripe.Event {
		raw, _ := json.Marshal(map[string]any{"id": "ch_1", "object": "charge", "payment_intent": pi, "amount_refunded": refunded})
		return stripe.Event{ID: fmt.Sprintf("evt_c%d", n), Type: stripe.EventTypeChargeRefunded, Account: "acct_fake1", Data: &stripe.EventData{Raw: raw}}
	}
	state := f.rowState(t, refund)
	f.must(t, s.HandleStripeEvent(ctx, charge(2600, 1)))
	if strings.Contains(logs.String(), "WARN") {
		t.Fatalf("a matching charge.refunded warned: %s", logs.String())
	}
	f.must(t, s.HandleStripeEvent(ctx, charge(5200, 2)))
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), pi) {
		t.Fatalf("a disagreeing charge.refunded didn't warn naming the intent: %s", logs.String())
	}
	if got := f.rowState(t, refund); got != state {
		t.Fatalf("charge.refunded changed the refund: %s -> %s", state, got)
	}
	if n := f.count(t, `select count(*) from stripe_events where id like 'evt_c%'`); n != 2 {
		t.Fatalf("%d charge events recorded, want 2", n)
	}
}
