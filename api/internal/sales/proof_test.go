package sales_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

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
