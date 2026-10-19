package sales_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// abandoned is a pending order whose hold ended two hours ago, queued for
// settling by the sweep.
func abandoned(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake) (sales.Checkout, plugin.PaymentIntent) {
	t.Helper()
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.exec(t, `update checkouts set expires_at = now() - interval '2 hours' where id = $1`, c.ID)
	f.must(t, jobs.Sweep(context.Background(), s.Pool))
	return c, pi
}

func TestAbandonedPaymentIsCancelled(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	c, pi := abandoned(t, s, f, fake)
	f.must(t, jobs.Sweep(ctx, s.Pool)) // queued once
	if n := f.count(t, `select count(*) from jobs where kind = 'settle_order'`); n != 1 {
		t.Fatalf("%d settle jobs, want 1", n)
	}
	drain(t, s, &mailer.Memory{})
	if !fake.Cancelled(pi.ID) {
		t.Fatal("intent not cancelled at Stripe")
	}
	if _, _, st := f.orderOf(t, c.ID); st != "cancelled" {
		t.Fatalf("order %s", st)
	}
	if n := f.count(t, `select count(*) from payments where status = 'cancelled' and client_secret = ''`); n != 1 {
		t.Fatal("payment not cancelled, or its client secret kept")
	}
	got, err := s.GetCheckout(ctx, c.Token)
	if err != nil || got.Status != "cancelled" {
		t.Fatalf("checkout reads %q %v, want cancelled", got.Status, err)
	}
	// The order took no money, so organizers never see it.
	if n := f.count(t, `select count(*) from audit_log where entity_id = (select id from orders where checkout_id = $1)`, c.ID); n != 0 {
		t.Fatal("an unpaid abandoned order was audited")
	}
	if n := f.count(t, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d jobs left", n)
	}
}

// The webhook got lost, but the buyer paid: settling confirms, never cancels.
func TestAbandonedButPaidIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seatTaken bool
		order     string
	}{{"seat still free", false, "paid"}, {"seat taken since", true, "refunded"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
			c, pi := abandoned(t, s, f, fake)
			if tc.seatTaken {
				f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
			}
			fake.SetIntentStatus(pi.ID, "succeeded")
			drain(t, s, &mailer.Memory{})
			if fake.Cancelled(pi.ID) {
				t.Fatal("a paid intent was cancelled")
			}
			if _, _, st := f.orderOf(t, c.ID); st != tc.order {
				t.Fatalf("order %s, want %s", st, tc.order)
			}
			if n := f.count(t, `select count(*) from payments where status = 'succeeded' and client_secret = ''`); n != 1 {
				t.Fatal("payment not succeeded, or its client secret kept")
			}
			if amount, _ := fake.Refunded(pi.ID); (amount > 0) != tc.seatTaken {
				t.Fatalf("refunded %d", amount)
			}
		})
	}
}

// cancelCounter counts cancel calls: Stripe can, now and then, cancel an
// intent that's still processing, so the fake refusing isn't proof enough.
type cancelCounter struct {
	*payments.Fake
	n *atomic.Int32
}

func (c cancelCounter) CancelPaymentIntent(ctx context.Context, account, intent, key string) error {
	c.n.Add(1)
	return c.Fake.CancelPaymentIntent(ctx, account, intent, key)
}

func TestProcessingPaymentIsLeftToFinish(t *testing.T) {
	s, fake := newPaidService(t)
	var cancels atomic.Int32
	s.Payments = cancelCounter{fake, &cancels}
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	c, pi := abandoned(t, s, f, fake)
	fake.SetIntentStatus(pi.ID, "processing")
	drain(t, s, &mailer.Memory{})
	if n := cancels.Load(); n != 0 || fake.Cancelled(pi.ID) {
		t.Fatalf("a processing intent was cancelled (%d cancel calls)", n)
	}
	if _, _, st := f.orderOf(t, c.ID); st != "pending" {
		t.Fatalf("order %s, want pending", st)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'settle_order' and attempts = 1`); n != 1 {
		t.Fatal("the settle job isn't waiting to ask again")
	}
}

// The buyer finished paying just as we cancelled: Stripe refuses the cancel,
// and the next run confirms.
func TestCancelRefusedBecauseItSucceeded(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	c, pi := abandoned(t, s, f, fake)
	fake.SetIntentStatus(pi.ID, "requires_action")
	fake.FailCall("CancelPaymentIntent", &plugin.ProviderError{Op: "cancel payment intent", Refused: true, Status: 400, Code: "payment_intent_unexpected_state"})
	drain(t, s, &mailer.Memory{})
	if _, _, st := f.orderOf(t, c.ID); st != "pending" {
		t.Fatalf("order %s after a refused cancel, want pending", st)
	}
	fake.SetIntentStatus(pi.ID, "succeeded")
	f.exec(t, `update jobs set run_at = now() where kind = 'settle_order'`)
	drain(t, s, &mailer.Memory{})
	if fake.Cancelled(pi.ID) {
		t.Fatal("a paid intent was cancelled")
	}
	if _, _, st := f.orderOf(t, c.ID); st != "paid" {
		t.Fatalf("order %s, want paid", st)
	}
}

// A refusal that will only repeat (we lost access to the account) ends the
// job: a late payment is still refunded by the webhook.
func TestADefiniteRefusalStopsSettling(t *testing.T) {
	for name, err := range map[string]error{
		"no access": &plugin.ProviderError{Op: "cancel payment intent", Refused: true, NoAccess: true, Status: 403},
		"refused":   &plugin.ProviderError{Op: "cancel payment intent", Refused: true, Status: 400, Code: "parameter_invalid_empty"},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
			c, pi := abandoned(t, s, f, fake)
			fake.FailCall("CancelPaymentIntent", err)
			drain(t, s, &mailer.Memory{})
			if fake.Cancelled(pi.ID) {
				t.Fatal("cancelled after all")
			}
			if _, _, st := f.orderOf(t, c.ID); st != "pending" {
				t.Fatalf("order %s, want pending", st)
			}
			if n := f.count(t, `select count(*) from jobs`); n != 0 {
				t.Fatalf("%d jobs left: a refusal that will repeat was retried", n)
			}
		})
	}
}

// An order whose intent never got made has nothing to cancel at Stripe.
func TestAbandonedWithoutAPaymentIsCancelled(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	fake.FailNext(&plugin.ProviderError{Op: "payment intent", Status: 500})
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrPaymentsUnavailable) {
		t.Fatalf("pay: %v", err)
	}
	f.exec(t, `update checkouts set expires_at = now() - interval '2 hours' where id = $1`, c.ID)
	f.must(t, jobs.Sweep(context.Background(), s.Pool))
	drain(t, s, &mailer.Memory{})
	if _, _, st := f.orderOf(t, c.ID); st != "cancelled" {
		t.Fatalf("order %s, want cancelled", st)
	}
}

// A cancellation that lands while Pay is making the intent: Pay sees it as
// it stores the intent, cancels the intent itself and says so.
func TestPayRacingACancellationStopsItsIntent(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	tx, err := f.owner.Begin(ctx)
	f.must(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `select 1 from events where id = $1 for no key update`, f.event)
	f.must(t, err)
	done := make(chan error)
	go func() {
		_, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"})
		done <- err
	}()
	waitForLockWaiters(t, f.owner, 1)
	_, err = tx.Exec(ctx, `update events set status = 'cancelled' where id = $1`, f.event)
	f.must(t, err)
	f.must(t, tx.Commit(ctx))
	var conflict catalogue.ErrConflict
	if err := <-done; !errors.As(err, &conflict) || conflict.Code != "event_cancelled" {
		t.Fatalf("pay: %v, want event_cancelled", err)
	}
	pi, _ := fake.Latest()
	if !fake.Cancelled(pi.ID) {
		t.Fatal("the intent Pay made wasn't cancelled")
	}
	if n := f.count(t, `select count(*) from payments`); n != 0 {
		t.Fatalf("%d payments stored on a cancelled event", n)
	}
	if _, err := s.GetCheckout(ctx, c.Token); !errors.As(err, &conflict) || conflict.Code != "event_cancelled" {
		t.Fatalf("get checkout: %v, want event_cancelled", err)
	}
}

// cancelledElsewhere is Stripe after someone else cancelled the intent under
// another key: our cancel then gets payment_intent_unexpected_state.
type cancelledElsewhere struct{ *payments.Fake }

func (c cancelledElsewhere) CancelPaymentIntent(ctx context.Context, account, intent, key string) error {
	c.SetIntentStatus(intent, "canceled")
	return c.Fake.CancelPaymentIntent(ctx, account, intent, key)
}

// Cancelled already, under a key that isn't ours: Stripe refuses our cancel,
// and reading the intent again shows the job is done.
func TestCancelRefusedBecauseItWasCancelled(t *testing.T) {
	s, fake := newPaidService(t)
	s.Payments = cancelledElsewhere{fake}
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	c, _ := abandoned(t, s, f, fake)
	fake.FailCall("CancelPaymentIntent", &plugin.ProviderError{Op: "cancel payment intent", Refused: true, Status: 400, Code: "payment_intent_unexpected_state"})
	drain(t, s, &mailer.Memory{})
	if _, _, st := f.orderOf(t, c.ID); st != "cancelled" {
		t.Fatalf("order %s, want cancelled", st)
	}
	if n := f.count(t, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d jobs left", n)
	}
}
