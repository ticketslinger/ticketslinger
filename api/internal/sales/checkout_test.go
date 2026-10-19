package sales_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

func buy(items ...sales.Item) sales.CheckoutInput {
	return sales.CheckoutInput{Organizer: "acme", Event: "gala", Items: items}
}

// Spec success criterion 2, hold stage: 1,000 buyers at once for a quota of
// 100 get exactly 100 checkouts, and the rest are told what's left.
func TestOversellAtHoldTime(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500})
	// A fixed, modest pool: every buyer queues for a connection rather than
	// for the database's max_connections, the same on a laptop and in CI.
	cfg := s.Pool.Config()
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	var ok, soldOut, other atomic.Int64
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateCheckout(ctx, sales.CheckoutInput{Organizer: "acme", Event: "gala",
				Items: []sales.Item{{TicketTypeID: f.general, Quantity: 1}}})
			var so sales.SoldOut
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &so) && so.Available[0].Remaining == 0:
				soldOut.Add(1)
			default:
				other.Add(1)
				t.Log(err)
			}
		}()
	}
	wg.Wait()
	t.Logf("1,000 checkouts for 100 seats in %v", time.Since(start))
	if ok.Load() != 100 || soldOut.Load() != 900 || other.Load() != 0 {
		t.Fatalf("ok %d, sold out %d, other %d; want 100, 900, 0", ok.Load(), soldOut.Load(), other.Load())
	}
	var held int
	_ = testdb.Owner(t, s.Pool).QueryRow(context.Background(), `select sum(quantity) from holds`).Scan(&held)
	if held != 100 {
		t.Fatalf("holds sum to %d, want 100", held)
	}
}

func TestCheckoutReturnsTokenAndQuote(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500, taxBP: 825})
	ctx := context.Background()
	items := []sales.Item{{TicketTypeID: f.general, Quantity: 2}}
	c, err := s.CreateCheckout(ctx, buy(items...))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Token) != 43 {
		t.Fatalf("token %q is %d chars, want 43", c.Token, len(c.Token))
	}
	if d := time.Until(c.ExpiresAt) - 15*time.Minute; d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("expires at %v, want about 15 minutes from now", c.ExpiresAt)
	}
	if c.Status != "open" || c.Quote.TotalCents != 5612 || c.Quote.FeeMode != "pass" {
		t.Fatalf("status %q, total %d (%s), want open, 5612, pass", c.Status, c.Quote.TotalCents, c.Quote.FeeMode)
	}
	got, err := s.GetCheckout(ctx, c.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.ID || got.Status != "open" || got.Token != "" || len(got.Items) != 1 || got.Items[0] != items[0] ||
		got.Quote.TotalCents != 5612 || len(got.Quote.Tickets) != 2 || !got.ExpiresAt.Equal(c.ExpiresAt) {
		t.Fatalf("GetCheckout = %+v, want the created checkout %+v", got, c)
	}
	if _, err := s.GetCheckout(ctx, "nope"); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("unknown token: %v, want ErrNotFound", err)
	}
	var hash []byte
	f.must(t, f.owner.QueryRow(ctx, `select token_hash from checkouts where id = $1`, c.ID).Scan(&hash))
	if !bytes.Equal(hash, sales.HashToken(c.Token)) {
		t.Fatalf("stored %x, want sha256 of the token", hash)
	}
	// An expired cart says so.
	f.exec(t, `update checkouts set expires_at = now() - interval '1 minute' where id = $1`, c.ID)
	if got, err := s.GetCheckout(ctx, c.Token); err != nil || got.Status != "expired" {
		t.Fatalf("expired checkout: %q %v, want expired", got.Status, err)
	}
}

func TestEverySharedQuotaMustFit(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 9000, false)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	f.quota(t, f.org, f.event, "VIP room", 3, vip)

	_, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: vip, Quantity: 4}))
	if r := remaining(err); r == nil || r[vip] != 3 || r[f.general] != 10 {
		t.Fatalf("4 VIP: %v %v, want sold out with VIP 3, General 10", err, r)
	}
	if _, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: vip, Quantity: 3})); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 8}))
	if r := remaining(err); r == nil || r[f.general] != 7 || r[vip] != 0 {
		t.Fatalf("8 General after 3 VIP: %v %v, want sold out with General 7, VIP 0", err, r)
	}
}

func TestExpiredHoldsFreeTheirSeats(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	one := sales.Item{TicketTypeID: f.general, Quantity: 1}
	c, err := s.CreateCheckout(ctx, buy(one))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCheckout(ctx, buy(one)); remaining(err) == nil {
		t.Fatalf("second checkout while held: %v, want sold out", err)
	}
	f.exec(t, `update checkouts set expires_at = now() - interval '1 minute' where id = $1`, c.ID)
	if _, err := s.CreateCheckout(ctx, buy(one)); err != nil {
		t.Fatalf("after the hold expired: %v", err)
	}
}

func TestHiddenTicketsNeedAnUnlockingCode(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 9000, true)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	backstage := f.promo(t, "BACKSTAGE", "percent", 0)
	f.exec(t, `insert into promo_unlocks(organizer_id, event_id, promo_code_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, backstage, vip)
	f.promo(t, "TENOFF", "percent", 1000)

	in := buy(sales.Item{TicketTypeID: vip, Quantity: 1})
	if _, err := s.CreateCheckout(ctx, in); invalidCode(err) != "unknown_ticket_type" {
		t.Fatalf("hidden without a code: %v, want unknown_ticket_type", err)
	}
	in.PromoCode = "TENOFF"
	if _, err := s.CreateCheckout(ctx, in); invalidCode(err) != "unknown_ticket_type" {
		t.Fatalf("hidden with a code that doesn't unlock it: %v, want unknown_ticket_type", err)
	}
	// Sold out lists what this buyer may see: VIP only with the code that unlocks it.
	if _, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 5})); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 6}))
	if r := remaining(err); r == nil || r[f.general] != 5 || len(r) != 1 {
		t.Fatalf("sold out without a code: %v %v, want only General (5 left)", err, r)
	}
	unlocked := buy(sales.Item{TicketTypeID: vip, Quantity: 1}, sales.Item{TicketTypeID: f.general, Quantity: 5})
	unlocked.PromoCode = "BACKSTAGE"
	_, err = s.CreateCheckout(ctx, unlocked)
	if r := remaining(err); r == nil || r[f.general] != 5 || r[vip] != 5 || len(r) != 2 {
		t.Fatalf("sold out with the unlocking code: %v %v, want General and VIP (5 left each)", err, r)
	}
	// An access code for VIP does nothing for a General-only cart.
	generalOnly := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
	generalOnly.PromoCode = "BACKSTAGE"
	if _, err := s.CreateCheckout(ctx, generalOnly); invalidCode(err) != "promo_not_applicable" {
		t.Fatalf("access code on a cart it unlocks nothing in: %v, want promo_not_applicable", err)
	}
	in.PromoCode = "backstage"
	c, err := s.CreateCheckout(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromoCode != "BACKSTAGE" || c.Quote.SubtotalCents != 9000 || c.Quote.DiscountCents != 0 {
		t.Fatalf("access code: %q subtotal %d discount %d, want full price", c.PromoCode, c.Quote.SubtotalCents, c.Quote.DiscountCents)
	}
}

func TestPromoCodeRules(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2000})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 5000, false)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	spring := f.promo(t, "SPRING", "percent", 1000)
	f.exec(t, `update promo_codes set applies_to_all = false where id = $1`, spring)
	f.exec(t, `insert into promo_applies_to(organizer_id, event_id, promo_code_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, spring, vip)

	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1}, sales.Item{TicketTypeID: vip, Quantity: 1})
	in.PromoCode = "  spring "
	c, err := s.CreateCheckout(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromoCode != "SPRING" || c.Quote.DiscountCents != 500 {
		t.Fatalf("SPRING on VIP only: %q discount %d, want 500 (10%% of VIP)", c.PromoCode, c.Quote.DiscountCents)
	}
	// SPRING takes nothing off General: refused, and no use is spent.
	generalOnly := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
	generalOnly.PromoCode = "SPRING"
	_, err = s.CreateCheckout(ctx, generalOnly)
	var inv catalogue.ErrInvalid
	if !errors.As(err, &inv) || inv.Code != "promo_not_applicable" || inv.Field != "promo_code" {
		t.Fatalf("SPRING on General only: %v, want promo_not_applicable on promo_code", err)
	}
	var carts int
	f.must(t, f.owner.QueryRow(ctx, `select count(*) from checkouts where promo_code_id = $1`, spring).Scan(&carts))
	if carts != 1 {
		t.Fatalf("%d checkouts use SPRING, want 1", carts)
	}

	for _, window := range []string{
		`valid_from = now() + interval '1 day'`,
		`valid_until = now() - interval '1 minute'`,
	} {
		f.exec(t, `update promo_codes set valid_from = null, valid_until = null where id = $1`, spring)
		f.exec(t, `update promo_codes set `+window+` where id = $1`, spring)
		if _, err := s.CreateCheckout(ctx, in); invalidCode(err) != "promo_invalid" {
			t.Errorf("%s: %v, want promo_invalid", window, err)
		}
	}
	in.PromoCode = "NOSUCH"
	if _, err := s.CreateCheckout(ctx, in); invalidCode(err) != "promo_invalid" {
		t.Errorf("unknown code: %v, want promo_invalid", err)
	}

	// max_uses 2: one used, one held by c. A third is refused until c expires.
	f.exec(t, `update promo_codes set valid_from = null, valid_until = null, max_uses = 2, used_count = 1 where id = $1`, spring)
	in.PromoCode = "SPRING"
	if _, err := s.CreateCheckout(ctx, in); invalidCode(err) != "promo_used_up" {
		t.Fatalf("third use: %v, want promo_used_up", err)
	}
	f.exec(t, `update checkouts set expires_at = now() - interval '1 minute' where id = $1`, c.ID)
	if _, err := s.CreateCheckout(ctx, in); err != nil {
		t.Fatalf("after the held use expired: %v", err)
	}
}

func TestWhenTicketsAreOnSale(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500})
	ctx := context.Background()
	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
	cases := []struct {
		name, setup, undo, code string
	}{
		{"draft", `update events set status = 'draft' where id = $1`, `update events set status = 'live' where id = $1`, "not found"},
		{"ended", `update events set starts_at = now() - interval '2 days', ends_at = now() - interval '1 hour' where id = $1`,
			`update events set starts_at = now() + interval '6 days', ends_at = now() + interval '7 days' where id = $1`, "event_over"},
		{"not yet", `update ticket_types set sales_start = '2030-10-14 23:00:00+00' where event_id = $1`,
			`update ticket_types set sales_start = null where event_id = $1`, "sales_not_open"},
		{"closed", `update ticket_types set sales_end = now() - interval '1 hour' where event_id = $1`,
			`update ticket_types set sales_end = null where event_id = $1`, "sales_closed"},
	}
	for _, c := range cases {
		f.exec(t, c.setup, f.event)
		_, err := s.CreateCheckout(ctx, in)
		got := invalidCode(err)
		if errors.Is(err, catalogue.ErrNotFound) {
			got = "not found"
		}
		if got != c.code {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
		var inv catalogue.ErrInvalid
		if c.name == "not yet" && (!errors.As(err, &inv) || inv.Message != "General goes on sale on Oct 14, 2030, 7:00 PM.") {
			t.Errorf("not yet: message %q, want the start in New York time", inv.Message)
		}
		f.exec(t, c.undo, f.event)
	}
	if _, err := s.CreateCheckout(ctx, in); err != nil {
		t.Fatalf("back on sale: %v", err)
	}
	if _, err := s.CreateCheckout(ctx, sales.CheckoutInput{Organizer: "acme", Event: "nope", Items: in.Items}); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("unknown event: %v, want ErrNotFound", err)
	}
}

func TestQuantityRules(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1000, price: 2500})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 9000, false)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	f.exec(t, `update ticket_types set max_per_order = 100`)
	f.exec(t, `update ticket_types set max_per_order = 10 where id = $1`, f.general)
	g := func(n int) sales.Item { return sales.Item{TicketTypeID: f.general, Quantity: n} }
	v := func(n int) sales.Item { return sales.Item{TicketTypeID: vip, Quantity: n} }
	for _, c := range []struct {
		name  string
		items []sales.Item
		code  string
	}{
		{"nothing", nil, "no_items"},
		{"zero", []sales.Item{g(0)}, "no_items"},
		{"negative", []sales.Item{g(1), v(-1)}, "no_items"},
		{"over max", []sales.Item{g(11)}, "quantity_out_of_range"},
		{"same type twice", []sales.Item{g(1), g(1)}, "too_many_tickets"},
		{"101 across types", []sales.Item{g(10), v(91)}, "too_many_tickets"},
		{"one huge", []sales.Item{v(1 << 40)}, "too_many_tickets"},
	} {
		if _, err := s.CreateCheckout(ctx, buy(c.items...)); invalidCode(err) != c.code {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	_, err := s.CreateCheckout(ctx, buy(g(11)))
	var inv catalogue.ErrInvalid
	if !errors.As(err, &inv) || inv.Message != "You can buy between 1 and 10 General tickets in one order." {
		t.Errorf("message %q", inv.Message)
	}
	if _, err := s.CreateCheckout(ctx, buy(g(10), v(90))); err != nil {
		t.Fatalf("100 across types: %v", err)
	}
	f.exec(t, `update ticket_types set min_per_order = 4, max_per_order = 4 where id = $1`, vip)
	_, err = s.CreateCheckout(ctx, buy(v(3)))
	if !errors.As(err, &inv) || inv.Code != "quantity_out_of_range" || inv.Message != "You need exactly 4 VIP tickets in one order." {
		t.Errorf("min = max: %v %q", err, inv.Message)
	}
}

func TestCheckoutStaysInsideItsOrganizer(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500})
	ctx := context.Background()
	other := f.organizer(t, "other")
	_, theirs, _ := f.liveEvent(t, other, seedOpts{quota: 100, price: 2500})
	if _, err := s.CreateCheckout(ctx, sales.CheckoutInput{Organizer: "other", Event: "gala", Items: []sales.Item{{TicketTypeID: theirs, Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: theirs, Quantity: 1})); invalidCode(err) != "unknown_ticket_type" {
		t.Fatalf("other's ticket type in an acme checkout: %v, want unknown_ticket_type", err)
	}
	err := system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error {
		for _, q := range []string{
			`select count(*) from organizers where id = $1`,
			`select count(*) from events where organizer_id = $1`,
			`select count(*) from ticket_types where organizer_id = $1`,
			`select count(*) from quotas where organizer_id = $1`,
			`select count(*) from quota_ticket_types where organizer_id = $1`,
			`select count(*) from checkouts where organizer_id = $1`,
			`select count(*) from holds where organizer_id = $1`,
		} {
			var n int
			if err := tx.QueryRow(ctx, q, other).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("acme's system scope: %s = %d, want 0", q, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A quota linked to General while a checkout waits on General's quotas is
// counted: the checkout notices the set changed and starts over, rather than
// selling past the new quota or refusing a sale it has room for.
func TestQuotaAddedWhileWaitingIsCounted(t *testing.T) {
	for _, size := range []int{1, 5} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s := newService(t)
			f := seed(t, s.Pool, seedOpts{quota: 100, price: 2500})
			ctx := context.Background()
			var balcony catalogue.Quota
			added, release := make(chan struct{}), make(chan struct{})
			edit := make(chan error, 1)
			go func() {
				edit <- db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
					if balcony, err = catalogue.CreateQuota(ctx, tx, f.org, f.event, audit.Actor{User: f.user},
						catalogue.QuotaInput{Name: "Balcony", Size: size, TicketTypeIDs: []uuid.UUID{f.general}}); err != nil {
						return err
					}
					close(added)
					<-release
					return nil
				})
			}()
			select {
			case <-added:
			case err := <-edit:
				t.Fatalf("create quota: %v", err)
			}
			sale := make(chan error, 1)
			go func() {
				_, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
				sale <- err
			}()
			waitForLockWaiters(t, f.owner, 1)
			close(release)
			if err := <-edit; err != nil {
				t.Fatalf("create quota: %v", err)
			}
			err := <-sale
			if size == 1 {
				if remaining(err)[f.general] != 1 {
					t.Fatalf("2 General with a new quota of 1: %v, want sold out with 1 left", err)
				}
				return
			}
			var used int
			f.must(t, f.owner.QueryRow(ctx, `select quota_used($1, null)`, balcony.ID).Scan(&used))
			if err != nil || used != 2 {
				t.Fatalf("2 General with a new quota of 5: %v, Balcony uses %d; want a checkout and 2", err, used)
			}
		})
	}
}

// When the clock is read: after the locks, per statement. A checkout that
// waited on a quota past the end of sales is refused, and one that got in
// holds for 15 minutes from when it got the locks, not from when it began.
func TestClockIsReadAfterTheLocks(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	park := func(wait time.Duration, during func()) (sales.Checkout, time.Time, error) {
		gate, err := f.owner.Begin(ctx)
		f.must(t, err)
		defer func() { _ = gate.Rollback(ctx) }()
		_, err = gate.Exec(ctx, `select 1 from quotas where id = $1 for update`, f.hall)
		f.must(t, err)
		type result struct {
			c   sales.Checkout
			err error
		}
		done := make(chan result, 1)
		go func() {
			c, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
			done <- result{c, err}
		}()
		waitForLockWaiters(t, f.owner, 1)
		during()
		time.Sleep(wait)
		released := time.Now()
		f.must(t, gate.Commit(ctx))
		r := <-done
		return r.c, released, r.err
	}
	_, _, err := park(400*time.Millisecond, func() {
		f.exec(t, `update ticket_types set sales_end = clock_timestamp() + interval '200 milliseconds' where id = $1`, f.general)
	})
	if invalidCode(err) != "sales_closed" {
		t.Fatalf("sales ended while waiting: %v, want sales_closed", err)
	}
	f.exec(t, `update ticket_types set sales_end = null where id = $1`, f.general)
	c, released, err := park(2*time.Second, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if early := released.Add(15 * time.Minute).Sub(c.ExpiresAt); early > 500*time.Millisecond {
		t.Fatalf("hold expires %v before 15 minutes from getting the locks", early)
	}
}

// One cart across a shared quota counts every type in it.
func TestOneCartAcrossASharedQuota(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 9000, false)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	_, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 6}, sales.Item{TicketTypeID: vip, Quantity: 5}))
	if r := remaining(err); r == nil || r[f.general] != 10 || r[vip] != 10 {
		t.Fatalf("6 General + 5 VIP in Hall 10: %v %v, want sold out with 10 left of each", err, r)
	}
	if _, err := s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: f.general, Quantity: 5}, sales.Item{TicketTypeID: vip, Quantity: 5})); err != nil {
		t.Fatalf("5 + 5 in Hall 10: %v", err)
	}
}

// Every catalogue write that takes quota or promo locks, racing a checkout
// parked while it holds them (foreign keys take FOR KEY SHARE on the event
// the edit holds). Neither side deadlocks: the cart goes in and the edit
// sees it, or the edit never needed what the cart holds.
func TestCatalogueEditsAndAHoldDontDeadlock(t *testing.T) {
	type edit func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error
	type tc struct {
		run edit
		// gate is the row the checkout is parked on: "promo" parks it after
		// its quota locks, "ticket type" after its promo lock too.
		gate string
		want string // the edit's 409 code; "" when it succeeds
	}
	ttPatch := func(p catalogue.TicketTypePatch) edit {
		return func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, a, p)
			return err
		}
	}
	cases := map[string]tc{
		"ticket type price": {ttPatch(catalogue.TicketTypePatch{PriceCents: ptr(int64(3000))}), "promo", "has_sales"},
		"ticket type min":   {ttPatch(catalogue.TicketTypePatch{MinPerOrder: ptr(2)}), "promo", "has_sales"},
		"ticket type max":   {ttPatch(catalogue.TicketTypePatch{MaxPerOrder: ptr(5)}), "promo", "has_sales"},
		"sales window": {ttPatch(catalogue.TicketTypePatch{SalesEnd: nullable.NewNullableWithValue(time.Now().Add(24 * time.Hour))}),
			"promo", ""},
		"delete ticket type": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			return catalogue.DeleteTicketType(ctx, tx, f.org, f.event, f.general, a)
		}, "promo", "has_sales"},
		"create quota": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.CreateQuota(ctx, tx, f.org, f.event, a, catalogue.QuotaInput{Name: "Tiny", Size: 0, TicketTypeIDs: []uuid.UUID{f.general}})
			return err
		}, "promo", "quota_below_sales"},
		"shrink quota": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, a, catalogue.QuotaPatch{Size: ptr(0)})
			return err
		}, "promo", "quota_below_sales"},
		"relink quota": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, a, catalogue.QuotaPatch{TicketTypeIDs: &[]uuid.UUID{f.vip}})
			return err
		}, "promo", "has_sales"},
		"delete quota": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			return catalogue.DeleteQuota(ctx, tx, f.org, f.event, f.hall, a)
		}, "promo", "has_sales"},
		"event tax": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, a, catalogue.EventPatch{TaxRateBP: ptr(500)}, catalogue.PaymentsConnect)
			return err
		}, "promo", "has_sales"},
		"event to draft": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, a, catalogue.EventPatch{Status: ptr("draft")}, catalogue.PaymentsConnect)
			return err
		}, "promo", "has_sales"},
		"promo value": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			_, err := catalogue.UpdatePromo(ctx, tx, f.org, f.event, f.gate, a, catalogue.PromoPatch{Value: ptr(int64(2000))})
			return err
		}, "ticket type", "has_sales"},
		"delete promo": {func(ctx context.Context, tx pgx.Tx, f fixture, a audit.Actor) error {
			return catalogue.DeletePromo(ctx, tx, f.org, f.event, f.gate, a)
		}, "ticket type", "has_sales"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newService(t)
			f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
			ctx := context.Background()
			// VIP and a second quota, so deletes aren't refused for leaving
			// the event without tickets or capacity.
			f.vip = f.ticketType(t, f.org, f.event, "VIP", 9000, false)
			f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, f.vip)
			f.quota(t, f.org, f.event, "Venue", 100, f.general, f.vip)
			f.gate = f.promo(t, "GATE", "percent", 1000)

			gate, err := f.owner.Begin(ctx)
			f.must(t, err)
			defer func() { _ = gate.Rollback(ctx) }()
			if c.gate == "promo" {
				_, err = gate.Exec(ctx, `select 1 from promo_codes where id = $1 for update`, f.gate)
			} else {
				// Blocks the holds insert's key share on General, after the
				// checkout has locked the code and inserted its cart.
				_, err = gate.Exec(ctx, `select 1 from ticket_types where id = $1 for update`, f.general)
			}
			f.must(t, err)
			sale := make(chan error, 1)
			go func() {
				in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
				in.PromoCode = "GATE"
				_, err := s.CreateCheckout(ctx, in)
				sale <- err
			}()
			waitForLockWaiters(t, f.owner, 1)
			edited := make(chan error, 1)
			go func() {
				edited <- db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
					return c.run(ctx, tx, f, audit.Actor{User: f.user})
				})
			}()
			if c.want != "" {
				waitForLockWaiters(t, f.owner, 2) // the edit waits on what the checkout holds
			} else if err := <-edited; err != nil {
				t.Fatalf("edit that needs nothing the checkout holds: %v", err)
			}
			f.must(t, gate.Commit(ctx))

			saleErr := <-sale
			var editErr error
			if c.want != "" {
				editErr = <-edited
			}
			var pg *pgconn.PgError
			if errors.As(saleErr, &pg) || errors.As(editErr, &pg) || saleErr != nil {
				t.Fatalf("checkout: %v, edit: %v", saleErr, editErr)
			}
			if code := conflictCode(editErr); code != c.want {
				t.Fatalf("edit: %v, want %q", editErr, c.want)
			}
		})
	}
}

// Two buyers of different ticket types (so no quota lock between them) race
// for a code's last use. The second sees the first's cart once it gets the
// code's row: exactly one wins.
func TestLastPromoUseGoesOnce(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.ticketType(t, f.org, f.event, "VIP", 9000, false)
	f.quota(t, f.org, f.event, "VIP room", 10, vip)
	last := f.promo(t, "LAST", "percent", 1000)
	f.exec(t, `update promo_codes set max_uses = 1 where id = $1`, last)

	gate, err := f.owner.Begin(ctx)
	f.must(t, err)
	defer func() { _ = gate.Rollback(ctx) }()
	_, err = gate.Exec(ctx, `select 1 from promo_codes where id = $1 for update`, last)
	f.must(t, err)
	results := make(chan error, 2)
	for _, tt := range []uuid.UUID{f.general, vip} {
		go func() {
			in := buy(sales.Item{TicketTypeID: tt, Quantity: 1})
			in.PromoCode = "LAST"
			_, err := s.CreateCheckout(ctx, in)
			results <- err
		}()
	}
	waitForLockWaiters(t, f.owner, 2)
	f.must(t, gate.Commit(ctx))
	a, b := <-results, <-results
	if (a == nil) == (b == nil) || invalidCode(a)+invalidCode(b) != "promo_used_up" {
		t.Fatalf("two buyers for the last use: %v and %v, want one checkout and one promo_used_up", a, b)
	}
}
