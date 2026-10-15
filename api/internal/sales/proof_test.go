package sales_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// concurrently runs fn n times at once and returns what the successful runs
// returned. Sold out is the one failure expected; anything else fails t.
func concurrently[T any](t *testing.T, n int, fn func() (T, error)) []T {
	t.Helper()
	var mu sync.Mutex
	var out []T
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			v, err := fn()
			if errors.As(err, &sales.SoldOut{}) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// Spec success criterion 2, end to end: 1,000 concurrent checkouts against a
// quota of 100, payments and expiries racing new buyers, every webhook
// delivered twice. Valid tickets plus live holds always come to exactly 100,
// and every late payer who didn't fit is refunded.
func TestOversellProof(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500})
	ctx := context.Background()
	general := sales.CheckoutInput{Organizer: "acme", Event: "gala", Items: []sales.Item{{TicketTypeID: f.general, Quantity: 1}}}
	held := concurrently(t, 1000, func() (sales.Checkout, error) { return s.CreateCheckout(ctx, general) })
	if len(held) != 100 {
		t.Fatalf("%d holds, want 100", len(held))
	}
	intents := make([]string, 0, 100)
	for _, c := range held { // sequential: each Pay makes one intent
		if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "b@example.com"}); err != nil {
			t.Fatal(err)
		}
		pi, _ := fake.Latest()
		intents = append(intents, pi.ID)
	}
	expired := make([]uuid.UUID, 30)
	for i := range expired {
		expired[i] = held[i].ID
	}
	expire(t, f, expired...)
	var wg sync.WaitGroup
	var newcomers atomic.Int64
	for range 30 {
		wg.Go(func() {
			if _, err := s.CreateCheckout(ctx, general); err == nil {
				newcomers.Add(1)
			} else if !errors.As(err, &sales.SoldOut{}) {
				t.Error(err)
			}
		})
	}
	for i, pi := range intents {
		for range 2 { // every event delivered twice
			wg.Go(func() {
				if err := s.HandleStripeEvent(ctx, succeeded(pi, "acct_fake1", 2600, i)); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	var valid, live, paid, cancelled, refunds int
	q := testdb.Owner(t, s.Pool)
	f.must(t, q.QueryRow(ctx, `select count(*) from tickets where status = 'valid'`).Scan(&valid))
	f.must(t, q.QueryRow(ctx, `select coalesce(sum(h.quantity), 0) from holds h join checkouts c on c.id = h.checkout_id
		where c.status in ('open', 'paying') and c.expires_at > now()`).Scan(&live))
	f.must(t, q.QueryRow(ctx, `select count(*) filter (where status = 'paid'), count(*) filter (where status = 'cancelled') from orders`).Scan(&paid, &cancelled))
	f.must(t, q.QueryRow(ctx, `select count(*) from refunds where reason = 'late_payment'`).Scan(&refunds))
	if valid+live != 100 || valid != paid || paid+cancelled != 100 || cancelled != refunds || int64(cancelled) != newcomers.Load() {
		t.Fatalf("valid %d + held %d (want 100); paid %d, cancelled %d, late refunds %d, newcomers %d", valid, live, paid, cancelled, refunds, newcomers.Load())
	}
	t.Logf("valid %d, held %d, paid %d, cancelled and refunded %d, newcomers %d", valid, live, paid, cancelled, newcomers.Load())
}

// deliverRefunds sends Stripe's word on every refund it holds that we still
// call requested: refund.created saying pending, then refund.updated saying
// status, each event times over, all at once. Copies of one event share its
// id, as Stripe's redeliveries do.
func (f fixture) deliverRefunds(t *testing.T, s *sales.Service, status string, times int) {
	t.Helper()
	ctx := context.Background()
	type row struct {
		id              uuid.UUID
		attempt         int
		amount          int64
		stripeID        string
		intent, account string
	}
	rows, err := f.owner.Query(ctx, `select r.id, r.attempt, r.amount_cents, r.stripe_refund_id, p.payment_intent_id, p.stripe_account_id
		from refunds r join payments p on p.id = r.payment_id where r.status = 'requested' and r.stripe_refund_id is not null`)
	f.must(t, err)
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (x row, err error) {
		err = r.Scan(&x.id, &x.attempt, &x.amount, &x.stripeID, &x.intent, &x.account)
		return x, err
	})
	f.must(t, err)
	if len(due) == 0 {
		t.Fatal("no refunds waiting for Stripe's word")
	}
	var wg sync.WaitGroup
	for _, r := range due {
		pending := refundEvent(r.stripeID, r.intent, r.account, r.amount, "pending", ours(r.id, r.attempt), 0)
		pending.ID, pending.Type = "evt_"+r.stripeID+"_created", stripe.EventTypeRefundCreated
		final := refundEvent(r.stripeID, r.intent, r.account, r.amount, status, ours(r.id, r.attempt), 0)
		final.ID = "evt_" + r.stripeID + "_" + status
		for _, ev := range []stripe.Event{pending, final} {
			for range times {
				wg.Go(func() {
					if err := s.HandleStripeEvent(ctx, ev); err != nil {
						t.Error(err)
					}
				})
			}
		}
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

// Spec success criterion 6. A full refund, a one-ticket refund and an event
// cancellation each void exactly the tickets they name, give their seats
// back only once Stripe says the money went, and leave our books matching
// the fake Stripe's to the cent. Refunds race new sales, and every Stripe
// event arrives twice.
func TestRefundProof(t *testing.T) {
	s, fake := newPaidService(t)
	fake.RefundsPending(true) // as in live mode: Stripe's word comes by webhook
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	m := &mailer.Memory{}
	user := audit.Actor{User: f.user}
	a, piA, aTickets := f.paidOrder(t, s, fake, 2, 1)
	b, piB, bTickets := f.paidOrder(t, s, fake, 2, 2)
	_, piC, cTickets := f.paidOrder(t, s, fake, 2, 3) // left for the cancellation
	drain(t, s, m)                                    // order confirmations
	general := sales.CheckoutInput{Organizer: "acme", Event: "gala", Items: []sales.Item{{TicketTypeID: f.general, Quantity: 1}}}

	// 1. Refunds race 12 buyers. Voided seats stay taken until Stripe answers,
	//    so exactly the 4 never-sold seats go, whatever the interleaving.
	var wg sync.WaitGroup
	wg.Go(func() {
		asOwner(t, s, f, func(tx pgx.Tx) error {
			_, err := sales.RefundTickets(ctx, tx, f.org, f.event, a, sales.RefundRequest{FeeBack: true}, "organizer", user)
			return err
		})
	})
	wg.Go(func() {
		asOwner(t, s, f, func(tx pgx.Tx) error {
			_, err := sales.RefundTickets(ctx, tx, f.org, f.event, b, sales.RefundRequest{TicketIDs: bTickets[:1]}, "organizer", user)
			return err
		})
	})
	held := concurrently(t, 12, func() (sales.Checkout, error) { return s.CreateCheckout(ctx, general) })
	wg.Wait()
	if len(held) != 4 || f.quotaUsed(t) != 10 {
		t.Fatalf("%d holds while refunds are pending (want 4), quota used %d (want 10)", len(held), f.quotaUsed(t))
	}

	// 2. Stripe takes the refunds (pending), then says succeeded, twice each,
	//    all at once. Seats come back exactly once: 3 of them.
	drain(t, s, m)
	if n := f.count(t, `select count(*) from refunds where status = 'requested' and stripe_refund_id is not null`); n != 2 || f.quotaUsed(t) != 10 {
		t.Fatalf("%d refunds with Stripe (want 2), quota used %d before Stripe confirmed (want 10)", n, f.quotaUsed(t))
	}
	f.deliverRefunds(t, s, "succeeded", 2)
	drain(t, s, m) // fee steps and emails
	if f.quotaUsed(t) != 7 {
		t.Fatalf("quota used %d after the refunds went through, want 7", f.quotaUsed(t))
	}
	late := concurrently(t, 6, func() (sales.Checkout, error) { return s.CreateCheckout(ctx, general) })
	if len(late) != 3 {
		t.Fatalf("%d of the freed seats sold, want 3", len(late))
	}

	// 3. One holder pays now; their webhook arrives, twice, after the
	//    cancellation.
	_, err := s.Pay(ctx, late[0].Token, sales.Details{Email: "late@example.com"})
	f.must(t, err)
	piLate, _ := fake.Latest()
	asOwner(t, s, f, func(tx pgx.Tx) error {
		_, err := sales.CancelEvent(ctx, tx, f.org, f.event, "Gala", user)
		return err
	})
	for range 2 {
		wg.Go(func() {
			if err := s.HandleStripeEvent(ctx, succeeded(piLate.ID, "acct_fake1", late[0].Quote.TotalCents, 99)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	drain(t, s, m)
	if f.quotaUsed(t) != 9 { // C's 2 and B's last still taken; 6 holds; the late payer's hold gone
		t.Fatalf("quota used %d before Stripe confirmed the cancellation's refunds, want 9", f.quotaUsed(t))
	}
	f.deliverRefunds(t, s, "succeeded", 2)
	drain(t, s, m)

	// 4. The books. Every ticket is void, released, and on the refund that
	//    named it; only the 6 open holds still count.
	if n := f.count(t, `select count(*) from tickets where status = 'valid' or released_at is null`); n != 0 || f.quotaUsed(t) != 6 {
		t.Fatalf("%d tickets still valid or holding a seat, quota used %d (want 0 and 6)", n, f.quotaUsed(t))
	}
	want := map[uuid.UUID]string{bTickets[0]: "organizer/false", bTickets[1]: "event_cancelled/true"}
	for _, id := range aTickets {
		want[id] = "organizer/true"
	}
	for _, id := range cTickets {
		want[id] = "event_cancelled/true"
	}
	rows, err := f.owner.Query(ctx, `select t.id, r.reason || '/' || r.fee_refunded from tickets t join refunds r on r.id = t.refund_id`)
	f.must(t, err)
	got := map[uuid.UUID]string{}
	var id uuid.UUID
	var how string
	_, err = pgx.ForEachRow(rows, []any{&id, &how}, func() error { got[id] = how; return nil })
	f.must(t, err)
	if fmt.Sprint(got) != fmt.Sprint(want) || f.count(t, `select count(*) from tickets`) != len(want) {
		t.Fatalf("tickets by refund:\n got %v\nwant %v", got, want)
	}
	// Each refund is what its tickets cost, fee included when it went back.
	if n := f.count(t, `select count(*) from refunds r where r.reason <> 'late_payment' and r.amount_cents <> (select sum(t.charge_cents +
			case when r.fee_refunded then t.fee_cents else 0 end) from tickets t where t.refund_id = r.id)`); n != 0 {
		t.Fatalf("%d refunds don't add up to their tickets", n)
	}
	for _, o := range []struct {
		name        string
		pi          string
		amount, fee int64
	}{
		{"A, whole order, fee back", piA, 5200, 200},
		{"B, one ticket fee kept, then the rest on cancel", piB, 2500 + 2600, 100},
		{"C, cancelled", piC, 5200, 200},
		{"late payer, cancelled event", piLate.ID, late[0].Quote.TotalCents, late[0].Quote.FeeCents},
	} {
		gotAmount, gotFee := fake.Refunded(o.pi)
		var ours, ourFee int64
		f.must(t, f.owner.QueryRow(ctx, `select coalesce(sum(r.amount_cents), 0), coalesce(sum(r.fee_cents) filter (where r.fee_refunded_at is not null), 0)
			from refunds r join payments p on p.id = r.payment_id where p.payment_intent_id = $1 and r.status = 'succeeded'`, o.pi).Scan(&ours, &ourFee))
		if gotAmount != o.amount || gotFee != o.fee || ours != gotAmount || ourFee != gotFee {
			t.Errorf("%s: Stripe %d/%d, ours %d/%d, want %d/%d", o.name, gotAmount, gotFee, ours, ourFee, o.amount, o.fee)
		}
		// Refund by refund: the ids and amounts Stripe holds are ours exactly.
		list, err := fake.ListRefunds(ctx, "acct_fake1", o.pi)
		f.must(t, err)
		var theirs []string
		for _, r := range list {
			theirs = append(theirs, fmt.Sprintf("%s %d", r.ID, r.AmountCents))
		}
		rows, err := f.owner.Query(ctx, `select r.stripe_refund_id || ' ' || r.amount_cents from refunds r join payments p on p.id = r.payment_id
			where p.payment_intent_id = $1`, o.pi)
		f.must(t, err)
		mine, err := pgx.CollectRows(rows, pgx.RowTo[string])
		f.must(t, err)
		slices.Sort(theirs)
		slices.Sort(mine)
		if !slices.Equal(theirs, mine) {
			t.Errorf("%s: Stripe's refunds %v, ours %v", o.name, theirs, mine)
		}
	}
	var fees []string
	for _, r := range fake.FeeRefunds() {
		fees = append(fees, fmt.Sprintf("%s %s %d", r.PaymentIntentID, r.IdempotencyKey, r.AmountCents))
	}
	rows, err = f.owner.Query(ctx, `select p.payment_intent_id || ' appfee-' || r.id || '-' || r.attempt || ' ' || r.fee_cents
		from refunds r join payments p on p.id = r.payment_id where r.fee_cents > 0 and r.fee_refunded_at is not null`)
	f.must(t, err)
	ourFees, err := pgx.CollectRows(rows, pgx.RowTo[string])
	f.must(t, err)
	slices.Sort(fees)
	slices.Sort(ourFees)
	if !slices.Equal(fees, ourFees) {
		t.Errorf("fee refunds at Stripe %v, ours %v", fees, ourFees)
	}
	if n := f.count(t, `select count(*) from orders where status not in ('refunded', 'cancelled')`); n != 0 {
		t.Fatalf("%d orders not refunded", n)
	}
	if n, k := f.count(t, `select count(*) from refunds where status = 'succeeded' and stripe_refund_id is not null`), len(fake.Refunds()); n != 5 || k != 5 {
		t.Fatalf("%d succeeded refund rows with a Stripe id, %d refunds at Stripe; want 5 and 5", n, k)
	}
	if n := refundEmails(m); n != 5 {
		t.Fatalf("%d refund emails, want one per refund: 5", n)
	}
}
