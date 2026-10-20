package sales_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// Spec test 3: what the event page shows, times the quantity, less the
// checkout's all-in discount, is the checkout's total, the PaymentIntent's
// amount and the stored order's total, to the cent.
func TestShownEqualsCharged(t *testing.T) {
	for _, feeMode := range []string{"pass", "absorb"} {
		for _, inclusive := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/inclusive=%v", feeMode, inclusive), func(t *testing.T) {
				s, f, rec := payService(t, seedOpts{quota: 100, price: 2500, taxBP: 825})
				ctx := context.Background()
				f.exec(t, `update events set fee_mode = $2, prices_include_tax = $3 where id = $1`, f.event, feeMode, inclusive)
				odd := f.addVIP(t, 2165, false)
				f.promo(t, "TEN", "percent", 1000)

				ev, err := s.StorefrontEvent(ctx, "acme", "gala", "")
				f.must(t, err)
				allIn := map[uuid.UUID]int64{}
				for _, tt := range ev.TicketTypes {
					allIn[tt.ID] = tt.Shown.AllInCents
				}
				for _, tt := range []uuid.UUID{f.general, odd} {
					if allIn[tt] <= 0 {
						t.Fatalf("all_in_cents for %s = %d, want it priced", tt, allIn[tt])
					}
					for _, code := range []string{"", "TEN"} {
						in := buy(sales.Item{TicketTypeID: tt, Quantity: 2})
						in.PromoCode = code
						c := f.open(t, s, in)
						q := c.Quote
						if code == "TEN" && q.AllInDiscountCents == 0 {
							t.Fatalf("code %q took nothing off: %+v", code, q)
						}
						if got := 2*allIn[tt] - q.AllInDiscountCents; got != q.TotalCents {
							t.Fatalf("%s code %q: 2 × %d − %d = %d, checkout total %d", tt, code, allIn[tt], q.AllInDiscountCents, got, q.TotalCents)
						}
						for _, qt := range q.Tickets {
							if qt.AllInCents != allIn[tt] {
								t.Fatalf("quote ticket all-in %d, event page %d", qt.AllInCents, allIn[tt])
							}
						}
						before := len(rec.calls())
						ps, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", ExpectedTotalCents: &q.TotalCents})
						f.must(t, err)
						calls := rec.calls()
						if len(calls) != before+1 {
							t.Fatalf("%d intents, want %d", len(calls), before+1)
						}
						var stored int64
						f.must(t, f.owner.QueryRow(ctx, `select total_cents from orders where checkout_id = $1`, c.ID).Scan(&stored))
						in2 := calls[before]
						if in2.AmountCents != q.TotalCents || ps.AmountCents != q.TotalCents || stored != q.TotalCents || in2.ApplicationFeeCents != q.FeeCents {
							t.Fatalf("intent %d (fee %d), start %d, order %d; want %d (fee %d)",
								in2.AmountCents, in2.ApplicationFeeCents, ps.AmountCents, stored, q.TotalCents, q.FeeCents)
						}
					}
				}
			})
		}
	}
}

// Spec test 4: a total the buyer wasn't shown charges nothing and changes
// nothing; the right one, or none, pays as before.
func TestPayRefusesUnexpectedTotal(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500, taxBP: 825})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
	off := c.Quote.TotalCents - 1
	_, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", ExpectedTotalCents: &off})
	if conflictCode(err) != "price_changed" {
		t.Fatalf("err = %v, want price_changed", err)
	}
	var status string
	f.must(t, f.owner.QueryRow(ctx, `select status from checkouts where id = $1`, c.ID).Scan(&status))
	if n := f.count(t, `select count(*) from orders`); n != 0 || len(rec.calls()) != 0 || status != "open" {
		t.Fatalf("%d orders, %d intents, checkout %s; want none, none, open", n, len(rec.calls()), status)
	}
	if n := f.count(t, `select count(*) from holds where checkout_id = $1`, c.ID); n == 0 {
		t.Fatal("holds released, want them kept")
	}

	negative := int64(-1)
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", ExpectedTotalCents: &negative}); invalidCode(err) != "bad_total" || fieldOf(err) != "expected_total_cents" {
		t.Fatalf("negative: err = %v, want bad_total on expected_total_cents", err)
	}

	right := c.Quote.TotalCents
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", ExpectedTotalCents: &right}); err != nil {
		t.Fatal(err)
	}
	c2 := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if _, err := s.Pay(ctx, c2.Token, sales.Details{Email: "buyer@example.com"}); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.calls()); n != 2 {
		t.Fatalf("%d intents, want 2", n)
	}
}
