package sales_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// newPaidService sells through the fake provider; seed gives acme acct_fake1
// with charges enabled.
func newPaidService(t *testing.T) (*sales.Service, *payments.Fake) {
	t.Helper()
	s := newService(t)
	fake := payments.NewFake()
	s.Payments = fake
	return s, fake
}

// succeeded is the payment_intent.succeeded Stripe sends for pi, as event
// evt_<n>. Two deliveries of one payment share n.
func succeeded(pi, account string, amount int64, n int) stripe.Event {
	return piEvent(pi, account, amount, "usd", n)
}

func piEvent(pi, account string, amount int64, currency string, n int) stripe.Event {
	raw, _ := json.Marshal(map[string]any{"id": pi, "object": "payment_intent", "amount": amount, "currency": currency, "status": "succeeded"})
	return stripe.Event{ID: fmt.Sprintf("evt_%d", n), Type: stripe.EventTypePaymentIntentSucceeded, Account: account, Data: &stripe.EventData{Raw: raw}}
}

// paying opens a checkout for items and pays: a pending order and its intent.
func paying(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, d sales.Details, items ...sales.Item) (sales.Checkout, plugin.PaymentIntent) {
	t.Helper()
	return payingWith(t, s, f, fake, buy(items...), d)
}

func payingWith(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, in sales.CheckoutInput, d sales.Details) (sales.Checkout, plugin.PaymentIntent) {
	t.Helper()
	c := f.open(t, s, in)
	if d.Email == "" {
		d.Email = "buyer@example.com"
	}
	_, err := s.Pay(context.Background(), c.Token, d)
	f.must(t, err)
	pi, _ := fake.Latest()
	return c, pi
}

// expire ends these checkouts' holds a minute ago.
func expire(t *testing.T, f fixture, ids ...uuid.UUID) {
	t.Helper()
	f.exec(t, `update checkouts set expires_at = now() - interval '1 minute' where id = any($1)`, ids)
}

func (f fixture) orderOf(t *testing.T, checkout uuid.UUID) (id uuid.UUID, code, status string) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), `select id, code, status from orders where checkout_id = $1`, checkout).Scan(&id, &code, &status))
	return id, code, status
}

func TestPaidWebhookCutsTicketsAndEmailsALink(t *testing.T) {
	s, fake := newPaidService(t)
	s.StorefrontURL = "http://localhost:3001"
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500, taxBP: 825})
	ctx := context.Background()
	promo := f.promo(t, "TEN", "percent", 1000)
	diet := f.question(t, "Dietary needs", "text", "ticket", false, nil)
	company := f.question(t, "Company", "text", "order", false, nil)
	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 2})
	in.PromoCode = "TEN"
	c, pi := payingWith(t, s, f, fake, in, sales.Details{Email: "buyer@example.com", Attendees: []string{"Ada", "Bob"},
		Answers: []sales.Answer{{QuestionID: diet, TicketIndex: 1, Value: "Vegan"}, {QuestionID: company, TicketIndex: -1, Value: "Acme"}}})
	if pi.AmountCents != c.Quote.TotalCents {
		t.Fatalf("intent for %d, want %d", pi.AmountCents, c.Quote.TotalCents)
	}
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))

	order, code, status := f.orderOf(t, c.ID)
	if status != "paid" {
		t.Fatalf("order %s, want paid", status)
	}
	rows, err := f.owner.Query(ctx, `select secret, price_cents, discount_cents, tax_cents, fee_cents, attendee_name, status
		from tickets where order_id = $1 order by id`, order)
	f.must(t, err)
	secrets := map[string]bool{}
	i := 0
	for ; rows.Next() && i < 2; i++ {
		var secret []byte
		var price, disc, tax, fee int64
		var name, st string
		f.must(t, rows.Scan(&secret, &price, &disc, &tax, &fee, &name, &st))
		want := c.Quote.Tickets[i]
		if len(secret) != 16 || price != want.PriceCents || disc != want.DiscountCents || tax != want.TaxCents || fee != want.FeeCents ||
			name != []string{"Ada", "Bob"}[i] || st != "valid" || disc == 0 {
			t.Errorf("ticket %d: %d-byte secret, %d/%d/%d/%d %q %s, want the quote's %+v", i, len(secret), price, disc, tax, fee, name, st, want)
		}
		secrets[string(secret)] = true
	}
	rows.Close()
	if n := f.count(t, `select count(*) from tickets where order_id = $1`, order); n != 2 {
		i = n
	}
	if i != 2 || len(secrets) != 2 {
		t.Fatalf("%d tickets with %d distinct secrets, want 2 and 2", i, len(secrets))
	}
	if n := f.count(t, `select count(*) from answers a join tickets t on t.id = a.ticket_id
		where a.order_id = $1 and a.question_id = $2 and a.value = 'Vegan' and t.attendee_name = 'Bob'`, order, diet); n != 1 {
		t.Errorf("Bob's ticket answer: %d rows, want 1", n)
	}
	if n := f.count(t, `select count(*) from answers where order_id = $1 and question_id = $2 and ticket_id is null and value = 'Acme'`, order, company); n != 1 {
		t.Errorf("order answer: %d rows, want 1", n)
	}
	if n := f.count(t, `select used_count from promo_codes where id = $1`, promo); n != 1 {
		t.Errorf("used_count %d, want 1", n)
	}
	if n := f.count(t, `select count(*) from holds where checkout_id = $1`, c.ID); n != 0 {
		t.Errorf("%d holds left", n)
	}
	if n := f.count(t, `select count(*) from checkouts where id = $1 and status = 'completed'`, c.ID); n != 1 {
		t.Error("checkout isn't completed")
	}
	if n := f.count(t, `select count(*) from payments where order_id = $1 and status = 'succeeded'`, order); n != 1 {
		t.Error("payment isn't succeeded")
	}

	// The job names the order; the link lives only in the email.
	token := sales.OrderToken(s.SecretKey, order)
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'order_confirmation'`).Scan(&payload))
	var job map[string]string
	f.must(t, json.Unmarshal(payload, &job))
	if len(job) != 2 || job["order_id"] != order.String() || job["organizer_id"] != f.org.String() ||
		strings.Contains(string(payload), token) || strings.Contains(string(payload), "?t=") {
		t.Fatalf("job payload %s, want only order_id and organizer_id", payload)
	}
	m := &mailer.Memory{}
	f.must(t, s.OrderConfirmation(m)(ctx, payload))
	link := regexp.MustCompile(`http://localhost:3001/orders/TS-[0-9A-HJKMNP-TV-Z]{5}\?t=[A-Za-z0-9_-]{43}`).FindString(m.LastLink("buyer@example.com"))
	if len(m.Sent) != 1 || link != "http://localhost:3001/orders/"+code+"?t="+token {
		t.Fatalf("email %+v, want the link to %s", m.Sent, code)
	}

	view, err := s.Order(ctx, strings.ToLower(code), token)
	f.must(t, err)
	if view.Code != code || view.Status != "paid" || view.Email != "buyer@example.com" || view.TotalCents != c.Quote.TotalCents ||
		view.Event.Name != "Gala" || view.Event.Organizer != "Acme" || len(view.Tickets) != 2 {
		t.Fatalf("order view %+v", view)
	}
	for i, tk := range view.Tickets {
		if len(tk.QR) != 22 || tk.TicketTypeName != "General" || tk.AttendeeName != []string{"Ada", "Bob"}[i] || tk.Status != "valid" {
			t.Errorf("ticket view %+v", tk)
		}
	}
	for name, try := range map[string][2]string{
		"wrong token":       {code, "wrong"},
		"empty token":       {code, ""},
		"another's token":   {code, sales.OrderToken(s.SecretKey, uuid.New())},
		"unknown code":      {"TS-00000", token},
		"not even a code":   {"TS-0000O", token},
		"token for the key": {code, sales.OrderToken([]byte("another key, another key, abcdef"), order)},
	} {
		if _, err := s.Order(ctx, try[0], try[1]); !errors.Is(err, errNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
}

// Spec success criterion 4: replays and parallel deliveries change nothing.
func TestReplayedEventChangesNothing(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	f.promo(t, "TEN", "percent", 1000)
	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 2})
	in.PromoCode = "TEN"
	c, pi := payingWith(t, s, f, fake, in, sales.Details{})
	ev := succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := s.HandleStripeEvent(ctx, ev); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range 3 {
		f.must(t, s.HandleStripeEvent(ctx, ev))
	}
	// A second event about the same payment finds the order paid already.
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 2)))
	if n := f.count(t, `select count(*) from orders where status = 'paid'`); n != 1 {
		t.Errorf("%d paid orders, want 1", n)
	}
	if n := f.count(t, `select count(*) from tickets`); n != 2 {
		t.Errorf("%d tickets, want 2", n)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation'`); n != 1 {
		t.Errorf("%d confirmation jobs, want 1", n)
	}
	if n := f.count(t, `select used_count from promo_codes`); n != 1 {
		t.Errorf("used_count %d, want 1", n)
	}
	if n := f.count(t, `select count(*) from stripe_events`); n != 2 {
		t.Errorf("%d events recorded, want 2", n)
	}
}

func TestWebhookFromAnotherAccountConfirmsNothing(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	f.organizer(t, "beta") // acct_fakebeta
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fakebeta", c.Quote.TotalCents, 1)))
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_nobody", c.Quote.TotalCents, 2)))
	if _, _, status := f.orderOf(t, c.ID); status != "pending" {
		t.Fatalf("order %s, want pending", status)
	}
	if n := f.count(t, `select count(*) from tickets`); n != 0 {
		t.Fatalf("%d tickets cut", n)
	}
}

// Confirm only on an exact match with our payments row.
func TestMismatchedPaymentConfirmsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		event func(pi string, total int64) stripe.Event
		setup func(t *testing.T, f fixture)
	}{
		"another intent":   {event: func(_ string, total int64) stripe.Event { return succeeded("pi_fakeother", "acct_fake1", total, 1) }},
		"another amount":   {event: func(pi string, total int64) stripe.Event { return succeeded(pi, "acct_fake1", total-1, 1) }},
		"another currency": {event: func(pi string, total int64) stripe.Event { return piEvent(pi, "acct_fake1", total, "eur", 1) }},
		// The organizer moved to a new account; the intent was made on the old one.
		"another account": {
			event: func(pi string, total int64) stripe.Event { return succeeded(pi, "acct_fakenew", total, 1) },
			setup: func(t *testing.T, f fixture) {
				f.exec(t, `update organizers set stripe_account_id = 'acct_fakenew' where id = $1`, f.org)
			},
		},
		"not succeeded": {event: func(pi string, total int64) stripe.Event {
			ev := succeeded(pi, "acct_fake1", total, 1)
			ev.Data.Raw = []byte(strings.Replace(string(ev.Data.Raw), `"succeeded"`, `"processing"`, 1))
			return ev
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
			c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
			if tc.setup != nil {
				tc.setup(t, f)
			}
			f.must(t, s.HandleStripeEvent(context.Background(), tc.event(pi.ID, c.Quote.TotalCents)))
			if _, _, status := f.orderOf(t, c.ID); status != "pending" {
				t.Fatalf("order %s, want pending", status)
			}
			if n := f.count(t, `select count(*) from tickets`) + f.count(t, `select count(*) from jobs`) +
				f.count(t, `select count(*) from payments where status <> 'pending'`); n != 0 {
				t.Fatalf("%d tickets, jobs or settled payments, want none", n)
			}
			if n := f.count(t, `select count(*) from stripe_events where id = 'evt_1'`); n != 1 {
				t.Fatal("the event wasn't recorded")
			}
		})
	}
}

func TestLatePaymentThatFitsStillSells(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	expire(t, f, c.ID)
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	if _, _, status := f.orderOf(t, c.ID); status != "paid" {
		t.Fatalf("order %s, want paid", status)
	}
	if n := f.count(t, `select count(*) from tickets where status = 'valid'`); n != 1 {
		t.Fatalf("%d tickets, want 1", n)
	}
}

// Whatever made the order unsellable, a paid order is cancelled, its payment
// marked succeeded, its holds freed, the system's decision audited, and a
// refund queued.
func TestPaidOrderThatCantBeSoldIsCancelledAndRefunded(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		quota int // 1 unless set
		in    func(t *testing.T, f fixture) sales.CheckoutInput
		d     func(t *testing.T, f fixture) sales.Details
		after func(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, c sales.Checkout)
	}{
		"seats gone": {after: func(t *testing.T, s *sales.Service, f fixture, _ *payments.Fake, c sales.Checkout) {
			expire(t, f, c.ID)
			f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1})) // takes the last seat
		}},
		"event no longer live": {after: func(t *testing.T, _ *sales.Service, f fixture, _ *payments.Fake, _ sales.Checkout) {
			f.exec(t, `update events set status = 'cancelled' where id = $1`, f.event) // the hold is still live
		}},
		"event over": {after: func(t *testing.T, _ *sales.Service, f fixture, _ *payments.Fake, c sales.Checkout) {
			expire(t, f, c.ID)
			f.exec(t, `update events set starts_at = now() - interval '2 hours', ends_at = now() - interval '1 hour' where id = $1`, f.event)
		}},
		"ticket type deleted": {after: func(t *testing.T, _ *sales.Service, f fixture, _ *payments.Fake, c sales.Checkout) {
			expire(t, f, c.ID)
			f.exec(t, `delete from ticket_types where id = $1`, f.general)
		}},
		"question deleted": {
			d: func(t *testing.T, f fixture) sales.Details {
				q := f.question(t, "Dietary needs", "text", "ticket", false, nil)
				return sales.Details{Answers: []sales.Answer{{QuestionID: q, TicketIndex: 0, Value: "Vegan"}}}
			},
			after: func(t *testing.T, _ *sales.Service, f fixture, _ *payments.Fake, _ sales.Checkout) {
				f.exec(t, `delete from questions`)
			},
		},
		"promo used up": {
			quota: 2,
			in: func(t *testing.T, f fixture) sales.CheckoutInput {
				f.exec(t, `insert into promo_codes(organizer_id, event_id, code, kind, value, max_uses) values ($1, $2, 'ONCE', 'percent', 1000, 1)`, f.org, f.event)
				in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
				in.PromoCode = "ONCE"
				return in
			},
			after: func(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, c sales.Checkout) {
				expire(t, f, c.ID)
				// Someone else uses the code's only use, and pays on time.
				in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
				in.PromoCode = "ONCE"
				c2, pi2 := payingWith(t, s, f, fake, in, sales.Details{Email: "other@example.com"})
				f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi2.ID, "acct_fake1", c2.Quote.TotalCents, 99)))
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: max(tc.quota, 1), price: 2500})
			in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
			if tc.in != nil {
				in = tc.in(t, f)
			}
			var d sales.Details
			if tc.d != nil {
				d = tc.d(t, f)
			}
			c, pi := payingWith(t, s, f, fake, in, d)
			tc.after(t, s, f, fake, c)
			f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))

			order, code, status := f.orderOf(t, c.ID)
			if status != "cancelled" {
				t.Fatalf("order %s, want cancelled", status)
			}
			if n := f.count(t, `select count(*) from tickets where order_id = $1`, order) + f.count(t, `select count(*) from answers where order_id = $1`, order); n != 0 {
				t.Errorf("%d tickets or answers written for a cancelled order", n)
			}
			if n := f.count(t, `select count(*) from payments where order_id = $1 and status = 'succeeded'`, order); n != 1 {
				t.Error("payment isn't succeeded")
			}
			if n := f.count(t, `select count(*) from holds where checkout_id = $1`, c.ID); n != 0 {
				t.Errorf("%d holds left", n)
			}
			var payload []byte
			f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'refund_payment'`).Scan(&payload))
			var job map[string]string
			f.must(t, json.Unmarshal(payload, &job))
			if len(job) != 2 || job["order_id"] != order.String() || job["organizer_id"] != f.org.String() {
				t.Errorf("refund job %s, want only order_id and organizer_id", payload)
			}
			if n := f.count(t, `select count(*) from audit_log where action = 'order.cancelled_late' and entity_id = $1
				and actor_system and actor_user_id is null and actor_api_key_id is null`, order); n != 1 {
				t.Errorf("%d system audit rows, want 1", n)
			}
			if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1`, order.String()); n != 0 {
				t.Error("a cancelled order was confirmed")
			}
			// The buyer's link shows what happened.
			view, err := s.Order(ctx, code, sales.OrderToken(s.SecretKey, order))
			f.must(t, err)
			if view.Status != "cancelled" || len(view.Tickets) != 0 {
				t.Errorf("order view %s with %d tickets", view.Status, len(view.Tickets))
			}
		})
	}
}

func TestRefundJobRefundsLatePayment(t *testing.T) {
	s, _ := newPaidService(t)
	rec := &refundRecorder{Fake: payments.NewFake()}
	s.Payments = rec
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, pi := paying(t, s, f, rec.Fake, sales.Details{Email: "late@example.com"}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	expire(t, f, c.ID)
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	order, _, _ := f.orderOf(t, c.ID)
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'refund_payment'`).Scan(&payload))

	refund := sales.RefundPayment(s)
	f.must(t, refund(ctx, payload))
	f.must(t, refund(ctx, payload))
	want := plugin.RefundInput{Account: "acct_fake1", PaymentIntentID: pi.ID, RefundApplicationFee: true, IdempotencyKey: "refund-" + order.String()}
	if calls := rec.calls(); len(calls) != 1 || calls[0] != want {
		t.Fatalf("refunds asked for %+v, want once %+v", calls, want)
	}
	var amount int64
	var fee bool
	var status, reason, stripeID string
	f.must(t, f.owner.QueryRow(ctx, `select r.amount_cents, r.fee_refunded, r.status, r.reason, r.stripe_refund_id
		from refunds r join payments p on p.id = r.payment_id where p.order_id = $1`, order).Scan(&amount, &fee, &status, &reason, &stripeID))
	if amount != c.Quote.TotalCents || !fee || status != "requested" || reason != "late_payment" || !strings.HasPrefix(stripeID, "re_fake") {
		t.Fatalf("refund row %d %v %s %s %s", amount, fee, status, reason, stripeID)
	}
	if n := f.count(t, `select count(*) from refunds`); n != 1 {
		t.Fatalf("%d refund rows, want 1", n)
	}
	if _, _, st := f.orderOf(t, c.ID); st != "refunded" {
		t.Fatalf("order %s, want refunded", st)
	}
	var mail []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'send_email'`).Scan(&mail))
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 1 {
		t.Fatalf("%d emails queued, want 1", n)
	}
	var msg struct{ To, Subject, Text string }
	f.must(t, json.Unmarshal(mail, &msg))
	if msg.To != "late@example.com" || msg.Subject != "Your refund for Gala" ||
		!strings.Contains(msg.Text, "couldn't keep your tickets for Gala") || !strings.Contains(msg.Text, "so we've refunded you in full: $26.00") ||
		strings.Contains(string(mail), "http") || strings.Contains(string(mail), sales.OrderToken(s.SecretKey, order)) {
		t.Fatalf("email %+v", msg)
	}
}

// refundRecorder is the fake provider, keeping every refund request.
type refundRecorder struct {
	*payments.Fake
	mu     sync.Mutex
	inputs []plugin.RefundInput
}

func (r *refundRecorder) Refund(ctx context.Context, in plugin.RefundInput) (string, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	r.mu.Unlock()
	return r.Fake.Refund(ctx, in)
}

func (r *refundRecorder) calls() []plugin.RefundInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.RefundInput(nil), r.inputs...)
}

// A replayed event changes nothing, even when replaying it would: here an
// older account.updated arriving again after a newer one.
func TestReplayedAccountUpdateChangesNothing(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	updated := func(id, object string, charges bool) stripe.Event {
		raw, _ := json.Marshal(map[string]any{"id": object, "object": "account", "charges_enabled": charges, "details_submitted": true})
		return stripe.Event{ID: id, Type: stripe.EventTypeAccountUpdated, Account: "acct_fake1", Data: &stripe.EventData{Raw: raw}}
	}
	charges := func() bool {
		var on bool
		f.must(t, f.owner.QueryRow(ctx, `select stripe_charges_enabled from organizers where id = $1`, f.org).Scan(&on))
		return on
	}
	f.must(t, s.HandleStripeEvent(ctx, updated("evt_on", "acct_fake1", true)))
	f.must(t, s.HandleStripeEvent(ctx, updated("evt_off", "acct_fake1", false)))
	f.must(t, s.HandleStripeEvent(ctx, updated("evt_on", "acct_fake1", true)))
	if charges() {
		t.Fatal("a replayed account.updated switched charges back on")
	}
	// An event about some other account's object is ignored.
	f.must(t, s.HandleStripeEvent(ctx, updated("evt_other", "acct_fakeother", true)))
	if charges() {
		t.Fatal("account.updated for another object changed acme")
	}
}

// Past Stripe's 24-hour idempotency window a retried refund finds the money
// already returned. That completes the job.
func TestRefundJobFinishesWhenStripeSaysAlreadyRefunded(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, pi := paying(t, s, f, fake, sales.Details{Email: "late@example.com"}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	expire(t, f, c.ID)
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'refund_payment'`).Scan(&payload))
	fake.FailNext(plugin.ErrAlreadyRefunded)
	f.must(t, sales.RefundPayment(s)(ctx, payload))
	if _, _, st := f.orderOf(t, c.ID); st != "refunded" {
		t.Fatalf("order %s, want refunded", st)
	}
	if n := f.count(t, `select count(*) from refunds where stripe_refund_id is null and status = 'requested'`) +
		f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 2 {
		t.Fatalf("%d of the refund row and the email, want both", n)
	}
	// Any other refusal is retried.
	f.exec(t, `update orders set status = 'cancelled'`)
	fake.FailNext(errors.New("stripe: refund: boom"))
	if err := sales.RefundPayment(s)(ctx, payload); err == nil {
		t.Fatal("a failed refund reported success")
	}
}

// The order page is one statement whether the link is right or not, so its
// timing doesn't say whether a code exists.
func TestOrderPageIsOneStatementHitOrMiss(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	order, code, _ := f.orderOf(t, c.ID)
	cfg := s.Pool.Config()
	counter := &statements{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	f.must(t, err)
	t.Cleanup(pool.Close)
	counted := *s
	counted.Pool = pool
	for name, try := range map[string][2]string{
		"hit":          {code, sales.OrderToken(s.SecretKey, order)},
		"wrong token":  {code, "wrong"},
		"unknown code": {"TS-00000", "wrong"},
	} {
		counter.n.Store(0)
		_, err := counted.Order(ctx, try[0], try[1])
		if name == "hit" && err != nil || name != "hit" && !errors.Is(err, errNotFound) {
			t.Errorf("%s: %v", name, err)
		}
		if n := counter.n.Load(); n != 1 {
			t.Errorf("%s: %d statements, want 1", name, n)
		}
	}
}

// statements counts the queries a pool sends.
type statements struct{ n atomic.Int64 }

func (c *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A question deleted between stillSellable's check and the answers' insert
// fails the foreign key. That rolls the whole event back, its stripe_events
// row too, so the webhook errors and Stripe retries; the retry finds the
// question gone, then cancels and refunds.
func TestQuestionDeletedMidConfirmIsRetriedIntoARefund(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	q := f.question(t, "Doomed", "text", "ticket", false, nil)
	c, pi := paying(t, s, f, fake, sales.Details{Answers: []sales.Answer{{QuestionID: q, TicketIndex: 0, Value: "Vegan"}}},
		sales.Item{TicketTypeID: f.general, Quantity: 1})
	// The race, made certain: inserting the tickets (after the check, before
	// the answers) deletes the question, as a DeleteQuestion committing then would.
	f.exec(t, `create function delete_doomed() returns trigger language plpgsql security definer
		set search_path = pg_catalog, public, pg_temp as $$
		begin delete from public.questions where label = 'Doomed'; return null; end $$`)
	f.exec(t, `create trigger race before insert on tickets for each statement execute function delete_doomed()`)
	ev := succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)
	err := s.HandleStripeEvent(ctx, ev)
	if err == nil || !strings.Contains(err.Error(), "23503") {
		t.Fatalf("first delivery: %v, want the answers' foreign key error", err)
	}
	if _, _, status := f.orderOf(t, c.ID); status != "pending" {
		t.Fatalf("after the failed delivery the order is %s, want pending", status)
	}
	if n := f.count(t, `select count(*) from stripe_events`) + f.count(t, `select count(*) from tickets`) + f.count(t, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d events, tickets or jobs kept from a rolled-back delivery", n)
	}
	// The deletion itself was rolled back too; now it really happens.
	f.exec(t, `drop trigger race on tickets`)
	f.exec(t, `delete from questions where id = $1`, q)
	f.must(t, s.HandleStripeEvent(ctx, ev))
	order, _, status := f.orderOf(t, c.ID)
	if status != "cancelled" {
		t.Fatalf("after the retry the order is %s, want cancelled", status)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'refund_payment' and payload->>'order_id' = $1`, order.String()); n != 1 {
		t.Fatalf("%d refund jobs, want 1", n)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'order.cancelled_late' and after->>'reason' = 'question_gone'`); n != 1 {
		t.Fatal("the cancellation doesn't say the question was gone")
	}
}
