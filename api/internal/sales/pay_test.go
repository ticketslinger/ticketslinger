package sales_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// recorder is the fake provider, keeping every PaymentIntent request.
type recorder struct {
	*payments.Fake
	mu     sync.Mutex
	inputs []plugin.PaymentIntentInput
}

func (r *recorder) CreatePaymentIntent(ctx context.Context, in plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	r.mu.Unlock()
	return r.Fake.CreatePaymentIntent(ctx, in)
}

func (r *recorder) calls() []plugin.PaymentIntentInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.PaymentIntentInput(nil), r.inputs...)
}

func payService(t *testing.T, o seedOpts) (*sales.Service, fixture, *recorder) {
	t.Helper()
	s := newService(t)
	rec := &recorder{Fake: payments.NewFake()}
	s.Payments = rec
	return s, seed(t, s.Pool, o), rec
}

func (f fixture) open(t *testing.T, s *sales.Service, in sales.CheckoutInput) sales.Checkout {
	t.Helper()
	c, err := s.CreateCheckout(context.Background(), in)
	f.must(t, err)
	return c
}

func (f fixture) count(t *testing.T, sql string, args ...any) (n int) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// question adds a question to gala; appliesTo empty means every ticket type.
func (f fixture) question(t *testing.T, label, kind, scope string, required bool, options []string, appliesTo ...uuid.UUID) uuid.UUID {
	t.Helper()
	if options == nil {
		options = []string{}
	}
	opts, _ := json.Marshal(options)
	q := f.id(t, `insert into questions(organizer_id, event_id, label, kind, options, required, scope, applies_to_all, position)
		values ($1, $2, $3, $4, $5, $6, $7, $8, (select count(*) + 1 from questions where event_id = $2)) returning id`,
		f.org, f.event, label, kind, opts, required, scope, len(appliesTo) == 0)
	for _, tt := range appliesTo {
		f.exec(t, `insert into question_applies_to(organizer_id, event_id, question_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, q, tt)
	}
	return q
}

func (f fixture) addVIP(t *testing.T, price int64, hidden bool) uuid.UUID {
	t.Helper()
	vip := f.ticketType(t, f.org, f.event, "VIP", price, hidden)
	f.exec(t, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, f.hall, vip)
	return vip
}

func expiresAt(t *testing.T, f fixture, id uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	f.must(t, f.owner.QueryRow(context.Background(), `select expires_at from checkouts where id = $1`, id).Scan(&at))
	return at
}

func TestPayCreatesOnePendingOrderAndIntent(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500, taxBP: 825})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
	before := expiresAt(t, f, c.ID)
	ps, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", Attendees: []string{"Ada", "Bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Kind != "test" || ps.ClientSecret == "" || ps.PublishableKey != "pk_test_fake" || ps.Account != "acct_fake1" || ps.AmountCents != c.Quote.TotalCents {
		t.Fatalf("start = %+v, want a test intent on acct_fake1 for %d", ps, c.Quote.TotalCents)
	}
	var order uuid.UUID
	var status, feeMode, email, code string
	var sub, disc, tax, fee, total int64
	var details []byte
	f.must(t, f.owner.QueryRow(ctx, `select id, code, status, fee_mode, email, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, details
		from orders where checkout_id = $1`, c.ID).Scan(&order, &code, &status, &feeMode, &email, &sub, &disc, &tax, &fee, &total, &details))
	q := c.Quote
	if status != "pending" || feeMode != "pass" || email != "buyer@example.com" ||
		sub != q.SubtotalCents || disc != q.DiscountCents || tax != q.TaxCents || fee != q.FeeCents || total != q.TotalCents {
		t.Fatalf("order %s %s %s %d/%d/%d/%d/%d, want pending pass with the quote's totals %+v", status, feeMode, email, sub, disc, tax, fee, total, q)
	}
	var d sales.Details
	f.must(t, json.Unmarshal(details, &d))
	if len(d.Attendees) != 2 || d.Attendees[1] != "Bob" {
		t.Fatalf("details = %s, want the attendee names", details)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("%d intents asked for, want 1", len(calls))
	}
	in := calls[0]
	if in.AmountCents != q.TotalCents || in.ApplicationFeeCents != q.FeeCents || in.IdempotencyKey != "pi-"+order.String() ||
		in.Metadata["order_id"] != order.String() || in.Account != "acct_fake1" || in.Description != "Order "+code+": 2 tickets" {
		t.Fatalf("intent input = %+v, want total %d, fee %d, key pi-%s", in, q.TotalCents, q.FeeCents, order)
	}
	var secret, provider string
	f.must(t, f.owner.QueryRow(ctx, `select client_secret, provider from payments where order_id = $1`, order).Scan(&secret, &provider))
	if secret != ps.ClientSecret || provider != "test" {
		t.Fatalf("payment row %q %q, want the intent's secret from the test provider", secret, provider)
	}
	var cstatus string
	f.must(t, f.owner.QueryRow(ctx, `select status from checkouts where id = $1`, c.ID).Scan(&cstatus))
	if after := expiresAt(t, f, c.ID); cstatus != "paying" || after.Sub(before) != 5*time.Minute {
		t.Fatalf("checkout %s, extended by %v; want paying, 5m", cstatus, after.Sub(before))
	}
}

func TestPayTwiceReusesThePaymentIntent(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	before := expiresAt(t, f, c.ID)
	first, err := s.Pay(ctx, c.Token, sales.Details{Email: "first@example.com"})
	f.must(t, err)
	second, err := s.Pay(ctx, c.Token, sales.Details{Email: "second@example.com"})
	f.must(t, err)
	if len(rec.calls()) != 1 || second.ClientSecret != first.ClientSecret || second.AmountCents != first.AmountCents {
		t.Fatalf("%d intents, secrets %q / %q; want one intent reused", len(rec.calls()), first.ClientSecret, second.ClientSecret)
	}
	if n := f.count(t, `select count(*) from orders where checkout_id = $1 and email = 'second@example.com'`, c.ID); n != 1 {
		t.Fatalf("%d orders with the new email, want 1", n)
	}
	if n := f.count(t, `select count(*) from orders`); n != 1 {
		t.Fatalf("%d orders, want 1", n)
	}
	if d := expiresAt(t, f, c.ID).Sub(before); d != 5*time.Minute {
		t.Fatalf("extended by %v over two calls, want 5m once", d)
	}
}

func TestPayRefusesTotalsUnderTheStripeMinimum(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 100})
	f.exec(t, `update events set fee_mode = 'absorb' where id = $1`, f.event)
	f.promo(t, "SIXTY", "percent", 6000)
	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
	in.PromoCode = "SIXTY"
	c := f.open(t, s, in)
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); invalidCode(err) != "below_minimum_charge" {
		t.Fatalf("$0.40 order: %v, want below_minimum_charge", err)
	}
	if n := f.count(t, `select count(*) from orders`); n != 0 {
		t.Fatalf("%d orders, want none", n)
	}
}

var orderCode = regexp.MustCompile(`^TS-[0-9A-HJKMNP-TV-Z]{5}$`)

func TestFreeOrderIsPaidAtOnce(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 0})
	ctx := context.Background()
	shirt := f.question(t, "Shirt size", "text", "ticket", true, nil)
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	ps, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", Attendees: []string{"Ada"},
		Answers: []sales.Answer{{QuestionID: shirt, TicketIndex: 0, Value: "M"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Kind != "free" || !orderCode.MatchString(ps.OrderCode) || !strings.Contains(ps.OrderURL, "/orders/"+ps.OrderCode+"?t=") {
		t.Fatalf("start = %+v, want free with an order code and link", ps)
	}
	var status string
	var paid *time.Time
	var hash []byte
	f.must(t, f.owner.QueryRow(ctx, `select status, paid_at, magic_token_hash from orders where code = $1`, ps.OrderCode).Scan(&status, &paid, &hash))
	u, err := url.Parse(ps.OrderURL)
	f.must(t, err)
	want := sha256.Sum256([]byte(u.Query().Get("t")))
	if status != "paid" || paid == nil || !bytes.Equal(hash, want[:]) {
		t.Fatalf("order %s paid_at %v hash %x; want paid, a time, sha256 of the link's token", status, paid, hash)
	}
	if n := f.count(t, `select count(*) from tickets where status = 'valid' and length(secret) = 16 and attendee_name = 'Ada'`); n != 1 {
		t.Fatalf("%d valid tickets with a 16-byte secret, want 1", n)
	}
	if n := f.count(t, `select count(*) from answers a join tickets t on t.id = a.ticket_id where a.question_id = $1 and a.value = 'M'`, shirt); n != 1 {
		t.Fatalf("%d answers on the ticket, want 1", n)
	}
	if n := f.count(t, `select count(*) from holds`); n != 0 {
		t.Fatalf("%d holds left, want none", n)
	}
	if n := f.count(t, `select count(*) from checkouts where status = 'completed'`); n != 1 || len(rec.calls()) != 0 {
		t.Fatalf("%d completed checkouts, %d intents; want 1 and none", n, len(rec.calls()))
	}
	// Paying again says it's done, and where.
	var done sales.CheckoutCompleted
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); !errors.As(err, &done) || done.OrderCode != ps.OrderCode {
		t.Fatalf("paying a completed checkout: %v, want CheckoutCompleted %s", err, ps.OrderCode)
	}
}

func TestFreeOrderEmailsItsLink(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 0})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	ps, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"})
	f.must(t, err)
	got, err := s.GetCheckout(ctx, c.Token)
	f.must(t, err)
	if got.Status != "completed" || got.OrderCode != ps.OrderCode || got.OrderURL != ps.OrderURL {
		t.Fatalf("GetCheckout = %s %s %s, want completed with %s", got.Status, got.OrderCode, got.OrderURL, ps.OrderURL)
	}
	// The job names the order; the link exists only in the email itself.
	u, err := url.Parse(got.OrderURL)
	f.must(t, err)
	token := u.Query().Get("t")
	if n := f.count(t, `select count(*) from jobs where payload::text like '%' || $1 || '%' or payload::text like '%?t=%'`, token); n != 0 || token == "" {
		t.Fatalf("%d jobs hold the order link or its token %q, want none", n, token)
	}
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'order_confirmation'`).Scan(&payload))
	var job map[string]string
	f.must(t, json.Unmarshal(payload, &job))
	if len(job) != 2 || job["organizer_id"] != f.org.String() || job["order_id"] == "" {
		t.Fatalf("job payload = %s, want only order_id and organizer_id", payload)
	}
	m := &mailer.Memory{}
	f.must(t, s.OrderConfirmation(m)(ctx, payload))
	if len(m.Sent) != 1 {
		t.Fatalf("%d emails sent, want 1", len(m.Sent))
	}
	mail := m.Sent[0]
	if mail.To != "buyer@example.com" || mail.Subject != "Your tickets for Gala" ||
		!strings.HasPrefix(mail.Text, "You're going to Gala.") || !strings.Contains(mail.Text, "Open your order: "+got.OrderURL+"\n") {
		t.Fatalf("email = %+v, want the link %s", mail, got.OrderURL)
	}
	// A partly refunded order still sends: its remaining tickets are the buyer's.
	f.exec(t, `update orders set status = 'partially_refunded'`)
	f.must(t, s.OrderConfirmation(m)(ctx, payload))
	if len(m.Sent) != 2 {
		t.Fatalf("%d emails after a partly refunded order, want 2", len(m.Sent))
	}
	m.Sent = m.Sent[:1]
	// An order that isn't paid sends nothing.
	f.exec(t, `update orders set status = 'cancelled'`)
	f.must(t, s.OrderConfirmation(m)(ctx, payload))
	if len(m.Sent) != 1 {
		t.Fatal("a cancelled order was emailed")
	}
	if !strings.HasPrefix(got.OrderURL, "https://tickets.example/orders/") {
		t.Fatalf("link %s isn't on the storefront", got.OrderURL)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	a, b := uuid.New(), uuid.New()
	first, again, other := sales.OrderToken(key, a), sales.OrderToken(key, a), sales.OrderToken(key, b)
	if first != again || first == other || len(first) != 43 {
		t.Fatal("orderToken must be 43 characters, stable for one order and different for two")
	}
	// Keyed by a key derived for order links, never TS_SECRET_KEY itself.
	raw := hmac.New(sha256.New, key)
	raw.Write(a[:])
	if first == base64.RawURLEncoding.EncodeToString(raw.Sum(nil)) {
		t.Fatal("orderToken uses TS_SECRET_KEY directly")
	}
}

func TestFreeCartNeedsNoStripe(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 0})
	s.Payments = nil
	f.exec(t, `update organizers set stripe_account_id = null, stripe_link_method = null, stripe_charges_enabled = false`)
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if ps, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); err != nil || ps.Kind != "free" {
		t.Fatalf("free order without Stripe: %+v %v", ps, err)
	}
}

// Buyers never hold seats nobody can pay for.
func TestCheckoutRefusesPaidTicketsStripeCantTake(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	in := buy(sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.exec(t, `update organizers set stripe_charges_enabled = false where id = $1`, f.org)
	if _, err := s.CreateCheckout(ctx, in); conflictCode(err) != "payments_not_ready" {
		t.Fatalf("charges off: %v, want payments_not_ready", err)
	}
	f.exec(t, `update organizers set stripe_account_id = null, stripe_link_method = null where id = $1`, f.org)
	if _, err := s.CreateCheckout(ctx, in); conflictCode(err) != "payments_not_ready" {
		t.Fatalf("no account: %v, want payments_not_ready", err)
	}
	f.exec(t, `update organizers set stripe_account_id = 'acct_fake1', stripe_link_method = 'created', stripe_charges_enabled = true where id = $1`, f.org)
	s.Payments = nil
	if _, err := s.CreateCheckout(ctx, in); !errors.Is(err, sales.ErrPaymentsNotConfigured) {
		t.Fatalf("no provider: %v, want ErrPaymentsNotConfigured", err)
	}
	if n := f.count(t, `select count(*) from checkouts`) + f.count(t, `select count(*) from holds`); n != 0 {
		t.Fatalf("%d checkouts and holds, want none", n)
	}
	// A code that makes the ticket free needs no Stripe.
	f.promo(t, "COMP", "percent", 10000)
	in.PromoCode = "COMP"
	if _, err := s.CreateCheckout(ctx, in); err != nil {
		t.Fatalf("free with a code, no provider: %v", err)
	}
}

func TestRequiredAnswers(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.addVIP(t, 9000, false)
	name := f.question(t, "Name on badge", "text", "ticket", true, nil)
	size := f.question(t, "Shirt size", "select", "order", false, []string{"S", "M"})
	diet := f.question(t, "Dinner choice", "text", "ticket", false, nil, vip)
	agree := f.question(t, "Photos OK", "checkbox", "order", false, nil)
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
	both := []sales.Answer{{QuestionID: name, TicketIndex: 0, Value: "Ada"}, {QuestionID: name, TicketIndex: 1, Value: "Bob"}}
	for label, tc := range map[string]struct {
		answers     []sales.Answer
		code, field string
	}{
		"one of two":          {both[:1], "missing_answer", "answers." + name.String() + ".1"},
		"blank":               {append([]sales.Answer{both[0]}, sales.Answer{QuestionID: name, TicketIndex: 1, Value: "  "}), "missing_answer", "answers." + name.String() + ".1"},
		"not an option":       {append(both, sales.Answer{QuestionID: size, TicketIndex: -1, Value: "XL"}), "bad_answer", "answers." + size.String() + ".-1"},
		"order q per ticket":  {append(both, sales.Answer{QuestionID: size, TicketIndex: 0, Value: "S"}), "bad_answer", "answers." + size.String() + ".0"},
		"VIP-only on General": {append(both, sales.Answer{QuestionID: diet, TicketIndex: 0, Value: "Fish"}), "bad_answer", "answers." + diet.String() + ".0"},
		"no such ticket":      {append(both, sales.Answer{QuestionID: name, TicketIndex: 2, Value: "Cy"}), "bad_answer", "answers." + name.String() + ".2"},
		"twice":               {append(both, both[0]), "bad_answer", "answers." + name.String() + ".0"},
		"checkbox":            {append(both, sales.Answer{QuestionID: agree, TicketIndex: -1, Value: "yes"}), "bad_answer", "answers." + agree.String() + ".-1"},
		"unknown question":    {append(both, sales.Answer{QuestionID: uuid.New(), TicketIndex: -1, Value: "x"}), "bad_answer", ""},
		"too long":            {append(both, sales.Answer{QuestionID: size, TicketIndex: -1, Value: strings.Repeat("a", 1001)}), "bad_answer", "answers." + size.String() + ".-1"},
	} {
		_, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", Answers: tc.answers})
		if invalidCode(err) != tc.code || tc.field != "" && fieldOf(err) != tc.field {
			t.Errorf("%s: %v (field %q), want %s on %s", label, err, fieldOf(err), tc.code, tc.field)
		}
	}
	for _, bad := range []string{"", "no-at-sign", "a@b", "Ada <ada@example.com>", strings.Repeat("a", 250) + "@example.com"} {
		if _, err := s.Pay(ctx, c.Token, sales.Details{Email: bad, Answers: both}); invalidCode(err) != "bad_email" || fieldOf(err) != "email" {
			t.Errorf("email %q: %v, want bad_email on email", bad, err)
		}
	}
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", Attendees: []string{"a", "b", "c"}, Answers: both}); invalidCode(err) != "bad_attendee" {
		t.Errorf("three names for two tickets: %v, want bad_attendee", err)
	}
	if n := f.count(t, `select count(*) from orders`); n != 0 {
		t.Fatalf("%d orders after refused details, want none", n)
	}
	ok := append(both, sales.Answer{QuestionID: size, TicketIndex: -1, Value: "M"}, sales.Answer{QuestionID: agree, TicketIndex: -1, Value: "true"})
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com", Answers: ok}); err != nil {
		t.Fatalf("complete answers: %v", err)
	}
}

func TestPayAfterExpiryIs410(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.exec(t, `update checkouts set expires_at = now() - interval '1 second' where id = $1`, c.ID)
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrCheckoutExpired) {
		t.Fatalf("expired: %v, want ErrCheckoutExpired", err)
	}
	if _, err := s.Pay(context.Background(), "nope", sales.Details{Email: "buyer@example.com"}); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown token: %v, want ErrNotFound", err)
	}
}

// A card may already have been charged once a payment exists: the hold
// running out mustn't read as "back on sale" while the webhook is on its way.
func TestPayingCheckoutStaysPayingPastItsHold(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `update checkouts set expires_at = now() - interval '1 second' where id = $1`, c.ID)
	if got, err := s.GetCheckout(ctx, c.Token); err != nil || got.Status != "paying" {
		t.Fatalf("paid-for checkout past its hold: %q %v, want paying", got.Status, err)
	}
	// Paying without a payment (the intent call failed) still runs out.
	f.exec(t, `delete from payments`)
	if got, err := s.GetCheckout(ctx, c.Token); err != nil || got.Status != "expired" {
		t.Fatalf("paying with no payment past its hold: %q %v, want expired", got.Status, err)
	}
}

func TestPaymentsNotReady(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	// Stripe switched charges off after the hold (account.updated).
	f.exec(t, `update organizers set stripe_charges_enabled = false where id = $1`, f.org)
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); conflictCode(err) != "payments_not_ready" {
		t.Fatalf("charges off: %v, want payments_not_ready", err)
	}
	if n := f.count(t, `select count(*) from orders`); n != 0 || len(rec.calls()) != 0 {
		t.Fatalf("%d orders, %d intents; want none", n, len(rec.calls()))
	}
	// Disconnected wins even over charges Stripe still has on.
	f.exec(t, `update organizers set stripe_charges_enabled = true, stripe_disconnected_at = now() where id = $1`, f.org)
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); conflictCode(err) != "payments_not_ready" {
		t.Fatalf("disconnected: %v, want payments_not_ready", err)
	}
	f.exec(t, `update organizers set stripe_disconnected_at = null where id = $1`, f.org)
	s.Payments = nil
	if _, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrPaymentsNotConfigured) {
		t.Fatalf("no provider: %v, want ErrPaymentsNotConfigured", err)
	}
}

func TestStripeDownLeavesTheHold(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
	rec.FailNext(errors.New("stripe: connection reset"))
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrPaymentsUnavailable) {
		t.Fatalf("Stripe down: %v, want ErrPaymentsUnavailable", err)
	}
	if n := f.count(t, `select count(*) from checkouts where status in ('open', 'paying') and expires_at > now()`); n != 1 {
		t.Fatal("the checkout should still be open or paying")
	}
	if n := f.count(t, `select coalesce(sum(quantity), 0) from holds`); n != 2 {
		t.Fatalf("holds = %d, want 2", n)
	}
	if n := f.count(t, `select count(*) from payments`); n != 0 {
		t.Fatalf("%d payments, want none", n)
	}
	// Stripe answering no is told apart from not answering; the hold stays too.
	rec.FailNext(&plugin.ProviderError{Op: "stripe: payment intent", Refused: true, Status: 400, Type: "invalid_request_error"})
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrPaymentsRefused) {
		t.Fatalf("Stripe refused: %v, want ErrPaymentsRefused", err)
	}
	ps, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"})
	if err != nil || ps.ClientSecret == "" {
		t.Fatalf("retry: %+v %v", ps, err)
	}
	calls := rec.calls()
	if len(calls) != 3 || calls[0].IdempotencyKey != calls[2].IdempotencyKey {
		t.Fatalf("intent requests %+v, want three with one idempotency key", calls)
	}
}

func TestConcurrentPaysShareOnePayment(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	var wg sync.WaitGroup
	starts := make([]sales.PaymentStart, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			starts[i], errs[i] = s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com"})
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || starts[0].ClientSecret == "" || starts[0].ClientSecret != starts[1].ClientSecret {
		t.Fatalf("two Pays at once: %v %v, secrets %q %q; want one shared payment", errs[0], errs[1], starts[0].ClientSecret, starts[1].ClientSecret)
	}
	if o, p := f.count(t, `select count(*) from orders`), f.count(t, `select count(*) from payments`); o != 1 || p != 1 {
		t.Fatalf("%d orders, %d payments; want 1 and 1", o, p)
	}
}

// The stored order is what gets charged or confirmed. If its totals stop
// matching the tickets, Pay refuses rather than charging (or giving away)
// an amount the buyer wasn't shown.
func TestPayRefusesWhenTheStoredTotalsDrift(t *testing.T) {
	s, f, rec := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.exec(t, `update organizers set stripe_charges_enabled = false`)
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); conflictCode(err) != "payments_not_ready" {
		t.Fatalf("setup: %v", err)
	}
	f.exec(t, `update organizers set stripe_charges_enabled = true`)
	_, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"})
	f.must(t, err)
	f.exec(t, `update orders set total_cents = 0, fee_cents = 0`)
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); conflictCode(err) != "price_changed" {
		t.Fatalf("drifted totals: %v, want price_changed", err)
	}
	if n := f.count(t, `select count(*) from orders where status = 'pending'`) + f.count(t, `select count(*) from tickets`); n != 1 || len(rec.calls()) != 1 {
		t.Fatalf("order still pending with no tickets and one intent? got %d, %d intents", n, len(rec.calls()))
	}
}

func TestGetCheckoutCarriesWhatThePageAsks(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.addVIP(t, 9000, false)
	name := f.question(t, "Name on badge", "text", "ticket", true, nil)
	f.question(t, "Dinner choice", "text", "ticket", false, nil, vip) // not asked of this cart
	size := f.question(t, "Shirt size", "select", "order", false, []string{"S", "M"})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 2}))
	got, err := s.GetCheckout(ctx, c.Token)
	f.must(t, err)
	e := got.Event
	if e == nil {
		t.Fatal("GetCheckout has no event")
	}
	if e.Organizer != "acme" || e.Slug != "gala" || e.Name != "Gala" || e.Timezone != "America/New_York" {
		t.Fatalf("event = %+v, want acme/gala, Gala, America/New_York", e)
	}
	if len(e.TicketTypes) != 1 || e.TicketTypes[0].ID != f.general || e.TicketTypes[0].Name != "General" {
		t.Fatalf("ticket types = %+v, want just General", e.TicketTypes)
	}
	if len(e.Questions) != 2 || e.Questions[0].ID != name || e.Questions[1].ID != size ||
		strings.Join(e.Questions[1].Options, ",") != "S,M" || !e.Questions[0].Required {
		t.Fatalf("questions = %+v, want Name on badge then Shirt size", e.Questions)
	}
}

// Connect and disconnect hold the organizer row across Stripe calls; a sale
// must never wait on that. Pay reads the row without a lock.
func TestPayDoesNotWaitOnTheOrganizerRow(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	hold, err := f.owner.Begin(ctx)
	f.must(t, err)
	defer func() { _ = hold.Rollback(ctx) }()
	_, err = hold.Exec(ctx, `select 1 from organizers where id = $1 for no key update`, f.org)
	f.must(t, err)
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ps, err := s.Pay(short, c.Token, sales.Details{Email: "buyer@example.com"})
	if err != nil || ps.ClientSecret == "" {
		t.Fatalf("pay while the organizer row is held: %+v %v", ps, err)
	}
	c2, err := s.CreateCheckout(short, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if err != nil || c2.Token == "" {
		t.Fatalf("checkout while the organizer row is held: %v", err)
	}
}
